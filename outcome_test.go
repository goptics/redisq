package redisq

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outcomeCase opens a producer and a consumer object of one queue kind on
// key, as two processes would.
type outcomeCase struct {
	name string
	open func(qs Queues, key string) (prod, cons *Queue, push func(item string) bool)
}

var outcomeCases = []outcomeCase{
	{"list", func(qs Queues, key string) (*Queue, *Queue, func(string) bool) {
		p, c := qs.NewQueue(key), qs.NewQueue(key)
		return p, c, func(v string) bool { return p.Enqueue(v) }
	}},
	{"priority", func(qs Queues, key string) (*Queue, *Queue, func(string) bool) {
		p, c := qs.NewPriorityQueue(key), qs.NewPriorityQueue(key)
		return p.Queue, c.Queue, func(v string) bool { return p.Enqueue(v, 1) }
	}},
}

// eachOutcomeCase runs fn for both kinds on a fresh key.
func eachOutcomeCase(t *testing.T, fn func(t *testing.T, prod, cons *Queue, push func(string) bool)) {
	for _, c := range outcomeCases {
		t.Run(c.name, func(t *testing.T) {
			qs := New(getTestRedisURL())
			defer qs.Close()
			prod, cons, push := c.open(qs, newID())
			defer prod.Close()
			defer cons.Close()
			fn(t, prod, cons, push)
		})
	}
}

// popAck pops the one waiting item from q and returns its ack ID.
func popAck(t *testing.T, q *Queue) string {
	t.Helper()
	var ackID string
	require.Eventually(t, func() bool {
		var ok bool
		if q.isPriority {
			_, ok, ackID = (&PriorityQueue{q}).DequeueWithAckId()
		} else {
			_, ok, ackID = q.DequeueWithAckId()
		}
		return ok
	}, 2*time.Second, 10*time.Millisecond)
	return ackID
}

// watch watches id on q and returns the channel fn sends each call to.
func watch(q *Queue, id string) (<-chan contract.JobOutcome, func()) {
	got := make(chan contract.JobOutcome, 4)
	cancel := q.WatchOutcome(id, func(o contract.JobOutcome) { got <- o })
	return got, cancel
}

func outcomeWithin(t *testing.T, got <-chan contract.JobOutcome, d time.Duration) contract.JobOutcome {
	t.Helper()
	select {
	case o := <-got:
		return o
	case <-time.After(d):
		t.Fatal("no outcome")
		return contract.JobOutcome{}
	}
}

// watchGone asserts q no longer watches id: no entry, no subscription.
func watchGone(t *testing.T, q *Queue, id string) {
	t.Helper()
	assert.Eventually(t, func() bool {
		q.outcomes.mu.Lock()
		_, held := q.outcomes.byID[id]
		q.outcomes.mu.Unlock()
		n, err := q.client.PubSubNumSub(context.Background(), q.donePrefix()+id).Result()
		return !held && err == nil && n[q.donePrefix()+id] == 0
	}, 2*time.Second, 20*time.Millisecond, "watch of %s still held", id)
}

func TestWatchOutcomeAcknowledge(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, _ := watch(prod, "j1")
		require.True(t, push(envelope("j1", `{"a":1}`)))
		require.True(t, cons.Acknowledge(popAck(t, cons)))

		o := outcomeWithin(t, got, time.Second) // a message, well before the sweep
		assert.Equal(t, contract.JobOutcome{ID: "j1", State: contract.JobCompleted}, o)
		watchGone(t, prod, "j1") // the watch ends by itself after fn
	})
}

func TestWatchOutcomeReject(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, _ := watch(prod, "j2")
		require.True(t, push(envelope("j2", `1`)))
		require.True(t, cons.Reject(popAck(t, cons), "boom"))

		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, contract.JobOutcome{ID: "j2", State: contract.JobFailed, Error: "boom"}, o)
	})
}

// A result keeps its exact bytes, big integers included, in the outcome
// and the record.
func TestAcknowledgeResultKeepsBytes(t *testing.T) {
	const result = `{"n":12345678901234567}`
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, _ := watch(prod, "j3")
		require.True(t, push(envelope("j3", `"x"`)))
		require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(result)))

		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, contract.JobCompleted, o.State)
		assert.Equal(t, result, string(o.Result))

		rec, err := cons.client.HGet(t.Context(), cons.getJobsKey(), "j3").Result()
		require.NoError(t, err)
		assert.Contains(t, rec, `"result":`+result)
		out, ok := outcomeOf("j3", rec)
		require.True(t, ok)
		assert.Equal(t, result, string(out.Result))

		s, err := cons.QueueStats(t.Context())
		require.NoError(t, err)
		assert.EqualValues(t, 1, s.Totals.Completed)
	})
}

// Invalid JSON is refused and leaves the job processing; the script's own
// check refuses what cjson can't decode before any write.
func TestAcknowledgeResultInvalidJSON(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		require.True(t, push(envelope("j4", `1`)))
		ackID := popAck(t, cons)
		assert.False(t, cons.AcknowledgeResult(ackID, []byte(`{"n":`)))
		assert.False(t, cons.AcknowledgeResult(ackID, nil))
		assert.Equal(t, 1, cons.GetNackedItemsCount(), "still processing")

		keys := []string{cons.getNackedItemKey(), cons.getTimeoutKey(), cons.getStartedKey(), cons.getAttemptsKey(), cons.getStatsKey(), cons.getJobsKey(), cons.getCompletedKey(), cons.getFailedKey(), cons.getPriorityKey()}
		r, err := finishScript.Run(t.Context(), cons.client, keys, ackID, "completed", "", 10, 60000, `{bad`, cons.donePrefix()).Int()
		require.NoError(t, err)
		assert.Equal(t, -1, r)
		assert.Equal(t, 1, cons.GetNackedItemsCount(), "still processing")
		assert.Zero(t, cons.client.HLen(t.Context(), cons.getJobsKey()).Val(), "no record")

		assert.True(t, cons.AcknowledgeResult(ackID, []byte(` [1, 2] `)))
		assert.Zero(t, cons.GetNackedItemsCount())
	})
}

// A job that ended before the watch is reported from its record.
func TestWatchOutcomeCatchUp(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		require.True(t, push(envelope("j5", `1`)))
		require.True(t, cons.Reject(popAck(t, cons), "late"))

		got, _ := watch(prod, "j5")
		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, contract.JobOutcome{ID: "j5", State: contract.JobFailed, Error: "late"}, o)
		watchGone(t, prod, "j5")
	})
}

// A lost message is made up by the sweep's read of the record.
func TestWatchOutcomeSweepCatchUp(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, _ := watch(prod, "j6")
		time.Sleep(50 * time.Millisecond) // the watch's own read finds nothing
		rec := `{"id":"j6","state":"completed","item":1,"finished_at":1,"attempts":1,"result":7}`
		require.NoError(t, cons.client.HSet(t.Context(), cons.getJobsKey(), "j6", rec).Err()) // no PUBLISH

		o := outcomeWithin(t, got, 3*time.Second)
		assert.Equal(t, `7`, string(o.Result))
	})
}

func TestWatchOutcomeCancel(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, cancel := watch(prod, "j7")
		cancel()
		cancel() // idempotent
		watchGone(t, prod, "j7")
		require.True(t, push(envelope("j7", `1`)))
		require.True(t, cons.Acknowledge(popAck(t, cons)))

		select {
		case <-got:
			t.Fatal("fn called after cancel")
		case <-time.After(1500 * time.Millisecond): // past a sweep
		}
	})
}

func TestWatchOutcomeTwoWatchers(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		var a, b atomic.Int32
		cancelA := prod.WatchOutcome("j8", func(contract.JobOutcome) { a.Add(1) })
		prod.WatchOutcome("j8", func(contract.JobOutcome) { b.Add(1) })
		require.True(t, push(envelope("j8", `1`)))
		require.True(t, cons.Acknowledge(popAck(t, cons)))

		require.Eventually(t, func() bool { return a.Load() == 1 && b.Load() == 1 }, time.Second, 10*time.Millisecond)
		cancelA()                           // after fn: no-op
		time.Sleep(1500 * time.Millisecond) // a sweep reads the record again
		assert.EqualValues(t, 1, a.Load())
		assert.EqualValues(t, 1, b.Load())
		watchGone(t, prod, "j8")
	})
}

// A job without an envelope id publishes nothing; an empty id watches
// nothing.
func TestOutcomeWithoutJobID(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		ps := cons.client.PSubscribe(t.Context(), cons.donePrefix()+"*")
		defer ps.Close()
		_, err := ps.Receive(t.Context()) // subscribed
		require.NoError(t, err)

		called := make(chan struct{}, 1)
		prod.WatchOutcome("", func(contract.JobOutcome) { called <- struct{}{} })()

		require.True(t, push("raw bytes"))
		require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(`1`)))
		require.True(t, push(`{"id":7}`)) // id not a string
		require.True(t, cons.Reject(popAck(t, cons), "x"))

		select {
		case m := <-ps.Channel():
			t.Fatalf("published %s", m.Channel)
		case <-called:
			t.Fatal("fn called for an empty id")
		case <-time.After(300 * time.Millisecond):
		}
	})
}

// Retention that keeps no record still publishes the outcome.
func TestOutcomePublishedWithoutRecord(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		cons.SetCompletedRetention(0, 0)
		got, _ := watch(prod, "j9")
		require.True(t, push(envelope("j9", `1`)))
		require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(`"r"`)))

		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, `"r"`, string(o.Result))
		assert.Zero(t, cons.client.HLen(t.Context(), cons.getJobsKey()).Val())
	})
}

// Close drops watchers without calling them; a watch after Close is a
// no-op.
func TestWatchOutcomeClose(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, cancel := watch(prod, "j10")
		require.NoError(t, prod.Close())
		cancel()
		late, _ := watch(prod, "j10")

		rec := `{"id":"j10","state":"completed","item":1,"finished_at":1,"attempts":1}`
		require.NoError(t, cons.client.HSet(t.Context(), cons.getJobsKey(), "j10", rec).Err())
		require.NoError(t, cons.client.Publish(t.Context(), cons.donePrefix()+"j10", `{"id":"j10","state":"completed"}`).Err())
		select {
		case <-got:
			t.Fatal("fn called after Close")
		case <-late:
			t.Fatal("fn of a watch made after Close called")
		case <-time.After(1500 * time.Millisecond):
		}
	})
}

// An id pushed again after its dedupe window drops the previous run's
// record, so a watch of the new run waits for it; a dedupe hit keeps it.
func TestEnqueueOnceDropsOldRecord(t *testing.T) {
	for _, c := range []struct {
		name string
		open func(qs Queues, key string) (prod, cons *Queue, pushOnce func(item, id string) bool)
	}{
		{"list", func(qs Queues, key string) (*Queue, *Queue, func(string, string) bool) {
			p, c := qs.NewQueue(key), qs.NewQueue(key)
			return p, c, func(v, id string) bool { return p.EnqueueOnce(v, id) }
		}},
		{"priority", func(qs Queues, key string) (*Queue, *Queue, func(string, string) bool) {
			p, c := qs.NewPriorityQueue(key), qs.NewPriorityQueue(key)
			return p.Queue, c.Queue, func(v, id string) bool { return p.EnqueueOnce(v, 1, id) }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			qs := New(getTestRedisURL())
			defer qs.Close()
			prod, cons, pushOnce := c.open(qs, newID())
			defer prod.Close()
			defer cons.Close()
			ctx := t.Context()
			record := func() bool { return cons.client.HExists(ctx, cons.getJobsKey(), "x").Val() }
			finished := func() int64 {
				return cons.client.ZCard(ctx, cons.getCompletedKey()).Val() + cons.client.ZCard(ctx, cons.getFailedKey()).Val()
			}

			require.True(t, pushOnce(envelope("x", `1`), "x"))
			require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(`"old"`)))
			require.True(t, record())

			require.True(t, pushOnce(envelope("x", `2`), "x")) // dedupe hit
			assert.True(t, record(), "a dedupe hit keeps the record")
			assert.EqualValues(t, 1, finished())

			require.NoError(t, cons.client.Del(ctx, cons.queueKey+":dedup:x").Err()) // the window passed
			require.True(t, pushOnce(envelope("x", `3`), "x"))
			assert.False(t, record(), "the push drops the old record")
			assert.Zero(t, finished())

			got, _ := watch(prod, "x")
			select {
			case o := <-got:
				t.Fatalf("stale outcome %+v", o)
			case <-time.After(1500 * time.Millisecond): // past a sweep
			}

			require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(`"new"`)))
			o := outcomeWithin(t, got, time.Second)
			assert.Equal(t, `"new"`, string(o.Result))
		})
	}
}
