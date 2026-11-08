package redisq

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cancelQueues are a producer and a consumer object of one queue kind on a
// fresh key; push adds item at priority p (ignored for fifo), pushOnce is
// EnqueueOnce.
type cancelQueues struct {
	prod, cons *Queue
	push       func(item string, p int) bool
	pushOnce   func(item, id string) bool
}

func eachCancelCase(t *testing.T, fn func(t *testing.T, c cancelQueues)) {
	for _, kind := range []string{"list", "priority"} {
		t.Run(kind, func(t *testing.T) {
			qs := New(getTestRedisURL())
			defer qs.Close()
			key := newID()
			var c cancelQueues
			if kind == "list" {
				p, q := qs.NewQueue(key), qs.NewQueue(key)
				c = cancelQueues{p, q,
					func(v string, _ int) bool { return p.Enqueue(v) },
					func(v, id string) bool { return p.EnqueueOnce(v, id) }}
			} else {
				p, q := qs.NewPriorityQueue(key), qs.NewPriorityQueue(key)
				c = cancelQueues{p.Queue, q.Queue,
					func(v string, prio int) bool { return p.Enqueue(v, prio) },
					func(v, id string) bool { return p.EnqueueOnce(v, 1, id) }}
			}
			defer c.prod.Close()
			defer c.cons.Close()
			fn(t, c)
		})
	}
}

// pops of q by kind, so the priority scripts run for a priority queue.
func rawPopOf(q *Queue) (any, bool) {
	if q.isPriority {
		return (&PriorityQueue{q}).Dequeue()
	}
	return q.Dequeue()
}

func ackPopOf(q *Queue) (any, bool, string) {
	if q.isPriority {
		return (&PriorityQueue{q}).DequeueWithAckId()
	}
	return q.DequeueWithAckId()
}

func batchPopOf(q *Queue, items []any, ackIDs []string) (int, int, error) {
	if q.isPriority {
		return (&PriorityQueue{q}).DequeueBatchWithAckIds(items, ackIDs)
	}
	return q.DequeueBatchWithAckIds(items, ackIDs)
}

// record reads job id's Job Record from q.
func record(t *testing.T, q *Queue, id string) map[string]any {
	t.Helper()
	s, err := q.client.HGet(t.Context(), q.getJobsKey(), id).Result()
	require.NoError(t, err, "record of %s", id)
	var r map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &r))
	return r
}

// assertDropped checks job id was dropped by a pop: record cancelled (no
// started_at, no errors), indexed in :failed, mark gone, nothing counted.
func assertDropped(t *testing.T, q *Queue, id string) {
	t.Helper()
	ctx := t.Context()
	r := record(t, q, id)
	assert.Equal(t, "cancelled", r["state"])
	assert.Equal(t, id, r["id"])
	assert.EqualValues(t, 1, r["attempts"])
	assert.NotContains(t, r, "started_at")
	assert.NotContains(t, r, "errors")
	assert.NotZero(t, r["finished_at"])
	assert.JSONEq(t, envelope(id, `1`), string(mustJSON(t, r["item"])))
	_, err := q.client.ZScore(ctx, q.getFailedKey(), id).Result()
	assert.NoError(t, err, "indexed in :failed")
	assert.False(t, marked(t, q, id), "mark gone")
	s := stats(t, q)
	assert.Zero(t, s.Totals.Completed)
	assert.Zero(t, s.Totals.Failed)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// A cancelled waiting job is never handed out, by any pop: dropped with
// record and outcome cancelled, and the pop goes on with the next item.
func TestCancelWaitingJob(t *testing.T) {
	pops := map[string]func(t *testing.T, q *Queue) []string{ // returns the ids handed out
		"raw": func(t *testing.T, q *Queue) []string {
			v, ok := rawPopOf(q)
			require.True(t, ok)
			return []string{envID(t, v)}
		},
		"single": func(t *testing.T, q *Queue) []string {
			v, ok, _ := ackPopOf(q)
			require.True(t, ok)
			return []string{envID(t, v)}
		},
		"batch one": func(t *testing.T, q *Queue) []string { // the lean batch script
			items, ids := make([]any, 1), make([]string, 1)
			n, _, err := batchPopOf(q, items, ids)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			return []string{envID(t, items[0])}
		},
	}
	for name, pop := range pops {
		t.Run(name, func(t *testing.T) {
			eachCancelCase(t, func(t *testing.T, c cancelQueues) {
				got, _ := watch(c.prod, "a")
				require.True(t, c.push(envelope("a", `1`), 1))
				require.True(t, c.push(envelope("b", `1`), 1))
				require.NoError(t, c.prod.CancelJob("a"))

				assert.Equal(t, []string{"b"}, pop(t, c.cons))
				assertDropped(t, c.cons, "a")
				assert.Equal(t, contract.JobOutcome{ID: "a", State: contract.JobCancelled, Error: "cancelled"}, outcomeWithin(t, got, time.Second))
				assert.Zero(t, c.cons.Len())
				assert.LessOrEqual(t, c.cons.GetNackedItemsCount(), 1, "only b")
				if c.cons.isPriority {
					assert.LessOrEqual(t, c.cons.client.HLen(t.Context(), c.cons.getPriorityKey()).Val(), int64(1))
				}
			})
		})
	}
}

// A batch pop drops marked items and pops more in their place, keeping
// order and each handed-out item's priority under its own ack ID.
func TestCancelBatchRefills(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
			require.True(t, c.push(envelope(id, `1`), i+1))
		}
		require.NoError(t, c.prod.CancelJob("a"))
		require.NoError(t, c.prod.CancelJob("c"))

		items, ackIDs := make([]any, 3), make([]string, 3)
		n, remaining, err := batchPopOf(c.cons, items, ackIDs)
		require.NoError(t, err)
		require.Equal(t, 3, n)
		assert.Equal(t, 1, remaining)
		var got []string
		for _, v := range items[:n] {
			got = append(got, envID(t, v))
		}
		assert.Equal(t, []string{"b", "d", "e"}, got)
		assertDropped(t, c.cons, "a")
		assertDropped(t, c.cons, "c")
		assert.Zero(t, c.cons.client.Exists(t.Context(), c.cons.getCancelledKey()).Val(), "last ZREM drops the key")
		assert.Equal(t, 3, c.cons.GetNackedItemsCount())

		if c.cons.isPriority {
			ctx := t.Context()
			prio := c.cons.client.HGetAll(ctx, c.cons.getPriorityKey()).Val()
			assert.Equal(t, map[string]string{ackIDs[0]: "2", ackIDs[1]: "4", ackIDs[2]: "5"}, prio)
		}
		for _, id := range ackIDs[:n] {
			require.True(t, c.cons.Acknowledge(id))
		}
	})
}

// With marks present, an item without an envelope id is handed out.
func TestCancelUnparsableHandedOut(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		require.NoError(t, c.prod.CancelJob("zzz"))
		require.True(t, c.push("raw bytes", 1))
		require.True(t, c.push(`{"id":7}`, 1))
		v, ok, _ := ackPopOf(c.cons)
		require.True(t, ok)
		assert.Equal(t, "raw bytes", string(v.([]byte)))
		v, ok, _ = ackPopOf(c.cons)
		require.True(t, ok)
		assert.Equal(t, `{"id":7}`, string(v.([]byte)))
	})
}

// A pop that drops the last waiting item finds the queue empty.
func TestCancelOnlyItem(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		c.cons.SetFailedRetention(0, 0)
		got, _ := watch(c.prod, "a")
		require.True(t, c.push(envelope("a", `1`), 1))
		require.NoError(t, c.prod.CancelJob("a"))
		_, ok, _ := ackPopOf(c.cons)
		assert.False(t, ok)
		assert.Zero(t, c.cons.client.HLen(t.Context(), c.cons.getJobsKey()).Val(), "no retention: no record")
		assert.Equal(t, contract.JobOutcome{ID: "a", State: contract.JobCancelled, Error: "cancelled"}, outcomeWithin(t, got, time.Second))
	})
}

// A job whose record shows it ended can't be cancelled; an unknown id is
// marked, with a TTL; an empty id is an error.
func TestCancelEndedAndUnknown(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		require.True(t, c.push(envelope("done", `1`), 1))
		require.True(t, c.cons.Acknowledge(popAck(t, c.cons)))
		assert.ErrorIs(t, c.prod.CancelJob("done"), contract.ErrJobEnded)

		require.True(t, c.push(envelope("bad", `1`), 1))
		require.True(t, c.cons.Reject(popAck(t, c.cons), "x"))
		assert.ErrorIs(t, c.prod.CancelJob("bad"), contract.ErrJobEnded)

		require.True(t, c.push(envelope("gone", `1`), 1))
		require.NoError(t, c.prod.CancelJob("gone"))
		_, ok, _ := ackPopOf(c.cons)
		require.False(t, ok)
		assert.ErrorIs(t, c.prod.CancelJob("gone"), contract.ErrJobEnded)
		assert.False(t, marked(t, c.cons, "gone"), "an ended job is not marked")

		require.NoError(t, c.prod.CancelJob("nobody"))
		assert.True(t, marked(t, c.cons, "nobody"))
		exp := c.cons.client.ZScore(ctx, c.cons.getCancelledKey(), "nobody").Val()
		now := float64(c.cons.client.Time(ctx).Val().UnixMilli())
		assert.InDelta(t, now+float64(cancelTTL.Milliseconds()), exp, 5000, "score: expiry in ms")
		ttl := c.cons.client.PTTL(ctx, c.cons.getCancelledKey()).Val()
		assert.Greater(t, ttl, 23*time.Hour, "backstop TTL")

		assert.Error(t, c.prod.CancelJob(""))
	})
}

// A running job's cancel reaches OnCancel in every process, in order; the
// consumer finishes it with AcknowledgeCancelled.
func TestCancelRunningJob(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		var mu sync.Mutex
		var seen []string
		stop := c.cons.OnCancel(func(id string) {
			mu.Lock()
			seen = append(seen, id)
			mu.Unlock()
		})
		calls := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }

		got, _ := watch(c.prod, "r")
		require.True(t, c.push(envelope("r", `1`), 1))
		ackID := popAck(t, c.cons)
		require.NoError(t, c.prod.CancelJob("r"))
		require.NoError(t, c.prod.CancelJob("x1"))
		require.NoError(t, c.prod.CancelJob("x2"))
		require.Eventually(t, func() bool { return len(calls()) == 3 }, time.Second, 5*time.Millisecond)
		assert.Equal(t, []string{"r", "x1", "x2"}, calls())

		require.True(t, c.cons.AcknowledgeCancelled(ackID, "stopped"))
		assert.Equal(t, contract.JobOutcome{ID: "r", State: contract.JobCancelled, Error: "stopped"}, outcomeWithin(t, got, time.Second))
		r := record(t, c.cons, "r")
		assert.Equal(t, "cancelled", r["state"])
		assert.Contains(t, r, "started_at")
		errs := r["errors"].([]any)
		require.Len(t, errs, 1)
		assert.Equal(t, "stopped", errs[0].(map[string]any)["error"])
		_, err := c.cons.client.ZScore(ctx, c.cons.getFailedKey(), "r").Result()
		assert.NoError(t, err, "indexed in :failed")
		assert.Zero(t, c.cons.GetNackedItemsCount())
		assert.False(t, marked(t, c.cons, "r"))
		assert.EqualValues(t, 1, stats(t, c.cons).Totals.Failed, "counted as Reject")
		assert.ErrorIs(t, c.prod.CancelJob("r"), contract.ErrJobEnded)

		// a catch-up read of the record gives the same outcome
		late, _ := watch(c.prod, "r")
		assert.Equal(t, contract.JobOutcome{ID: "r", State: contract.JobCancelled, Error: "stopped"}, outcomeWithin(t, late, time.Second))

		stop()
		require.NoError(t, c.prod.CancelJob("x3"))
		assert.Never(t, func() bool { return len(calls()) > 3 }, 200*time.Millisecond, 10*time.Millisecond, "no call after stop")
	})
}

// A job that ends another way despite its mark drops the mark.
func TestCancelFinishClearsMark(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		for _, finish := range []func(ackID string) bool{
			c.cons.Acknowledge,
			func(a string) bool { return c.cons.Reject(a, "x") },
			func(a string) bool { return c.cons.AcknowledgeResult(a, []byte(`1`)) },
		} {
			require.True(t, c.push(envelope("m", `1`), 1))
			ackID := popAck(t, c.cons)
			require.NoError(t, c.prod.CancelJob("m"))
			require.True(t, finish(ackID))
			assert.False(t, marked(t, c.cons, "m"))
			require.NoError(t, c.cons.client.HDel(ctx, c.cons.getJobsKey(), "m").Err()) // next round can mark it
		}
	})
}

// A watch that starts after a job was dropped gets the outcome from its
// record.
func TestCancelOutcomeCatchUp(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		require.True(t, c.push(envelope("a", `1`), 1))
		require.True(t, c.push(envelope("b", `1`), 1))
		require.NoError(t, c.prod.CancelJob("a"))
		popAck(t, c.cons)

		got, _ := watch(c.prod, "a")
		assert.Equal(t, contract.JobOutcome{ID: "a", State: contract.JobCancelled, Error: "cancelled"}, outcomeWithin(t, got, time.Second))
	})
}

// EnqueueOnce that pushes clears the mark, so the new job runs; a plain
// Enqueue doesn't. Purge deletes the marks.
func TestCancelReAddAndPurge(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		require.NoError(t, c.prod.CancelJob("n"))
		require.True(t, c.pushOnce(envelope("n", `1`), "n"))
		assert.False(t, marked(t, c.cons, "n"))
		v, ok, _ := ackPopOf(c.cons)
		require.True(t, ok)
		assert.Equal(t, "n", envID(t, v))

		require.NoError(t, c.prod.CancelJob("p"))
		require.True(t, c.pushOnce(envelope("p", `1`), "p"))
		require.NoError(t, c.prod.CancelJob("p"))
		require.True(t, c.pushOnce(envelope("p", `2`), "p")) // dedupe hit: mark stays
		assert.True(t, marked(t, c.cons, "p"))

		c.prod.Purge()
		assert.Zero(t, c.cons.client.Exists(ctx, c.cons.getCancelledKey()).Val())
	})
}

// An expired mark counts as none: the pop hands the item out and removes
// the mark. CancelJob and the sweep prune expired marks.
func TestCancelExpiredMark(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		key := c.cons.getCancelledKey()
		past := float64(c.cons.client.Time(ctx).Val().UnixMilli() - 1000)
		require.NoError(t, c.cons.client.ZAdd(ctx, key, redis.Z{Score: past, Member: "e"}).Err())
		require.True(t, c.push(envelope("e", `1`), 1))
		v, ok, _ := ackPopOf(c.cons)
		require.True(t, ok)
		assert.Equal(t, "e", envID(t, v))
		assert.False(t, c.cons.client.HExists(ctx, c.cons.getJobsKey(), "e").Val(), "no cancelled record")
		assert.Zero(t, c.cons.client.Exists(ctx, key).Val(), "the pop removed the expired mark")

		require.NoError(t, c.cons.client.ZAdd(ctx, key, redis.Z{Score: past, Member: "old"}).Err())
		require.NoError(t, c.prod.CancelJob("new"))
		assert.Equal(t, []string{"new"}, c.cons.client.ZRange(ctx, key, 0, -1).Val(), "CancelJob prunes")

		require.NoError(t, c.cons.client.ZAdd(ctx, key, redis.Z{Score: past, Member: "old"}).Err())
		assert.Eventually(t, func() bool {
			return c.cons.client.ZScore(ctx, key, "old").Err() == redis.Nil
		}, 3*time.Second, 50*time.Millisecond, "the sweep prunes")
		assert.True(t, marked(t, c.cons, "new"), "a live mark stays")
	})
}

// One script call drops at most maxPop marked items; the pop that ran out
// of budget leaves the rest, keeps the consumer out of :idle, and the next
// pop goes on.
func TestCancelDropBudget(t *testing.T) {
	const marks = maxPop + 500
	fill := func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		c.cons.Subscribe(func(string) {})
		exp := float64(c.cons.client.Time(ctx).Val().UnixMilli() + time.Hour.Milliseconds())
		z := make([]redis.Z, marks)
		for i := range marks {
			id := fmt.Sprintf("c%d", i)
			require.True(t, c.push(envelope(id, `1`), 1))
			z[i] = redis.Z{Score: exp, Member: id}
		}
		require.True(t, c.push(envelope("live", `1`), 1))
		require.NoError(t, c.cons.client.ZAdd(ctx, c.cons.getCancelledKey(), z...).Err())
	}
	check := func(t *testing.T, q *Queue, dropped int64, idle bool) {
		t.Helper()
		ctx := t.Context()
		assert.Equal(t, dropped, q.client.ZCard(ctx, q.getFailedKey()).Val())
		assert.Equal(t, dropped, q.client.HLen(ctx, q.getJobsKey()).Val())
		assert.Equal(t, idle, q.client.SIsMember(ctx, q.getIdleKey(), q.consumerID()).Val(), "idle")
	}
	extra := func(q *Queue) []string {
		if q.isPriority {
			return []string{q.getPriorityKey()}
		}
		return nil
	}

	t.Run("batch", func(t *testing.T) {
		eachCancelCase(t, func(t *testing.T, c cancelQueues) {
			fill(t, c)
			items, ids := make([]any, 2), make([]string, 2)
			n, remaining, err := batchPopOf(c.cons, items, ids)
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.Equal(t, marks-maxPop+1, remaining, "the rest is reported")
			check(t, c.cons, maxPop, false)

			n, remaining, err = batchPopOf(c.cons, items, ids)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			assert.Zero(t, remaining)
			assert.Equal(t, "live", envID(t, items[0]))
			check(t, c.cons, marks, true) // short of its slots with the queue empty
			assert.Equal(t, "cancelled", record(t, c.cons, "c0")["state"])
			assert.Equal(t, "cancelled", record(t, c.cons, fmt.Sprintf("c%d", marks-1))["state"])
		})
	})
	t.Run("single", func(t *testing.T) {
		eachCancelCase(t, func(t *testing.T, c cancelQueues) {
			fill(t, c)
			q := c.cons
			pop := listAckPopOne
			if q.isPriority {
				pop = zsetAckPopOne
			}
			ackID := q.ackIDOf(fmt.Sprintf("%020d", q.ackSeq.Add(1)), 1)
			args := append([]any{q.vt(), ackID, q.consumerID()}, append(q.cancelArgs(), q.hbt(), q.ackPrefix)...)
			r, err := pop.Run(t.Context(), q.client, q.popKeys(extra(q)...), args...).Result()
			require.NoError(t, err)
			assert.EqualValues(t, -1, r, "budget spent")
			check(t, q, maxPop, false)

			v, ok, _ := ackPopOf(q)
			require.True(t, ok)
			assert.Equal(t, "live", envID(t, v))
			check(t, q, marks, false)
		})
	})
	t.Run("raw", func(t *testing.T) {
		eachCancelCase(t, func(t *testing.T, c cancelQueues) {
			fill(t, c)
			v, ok := rawPopOf(c.cons) // pops again after a spent budget
			require.True(t, ok)
			assert.Equal(t, "live", envID(t, v))
			check(t, c.cons, marks, false)
		})
	})
}

// marked reports whether q's cancel marks hold id.
func marked(t *testing.T, q *Queue, id string) bool {
	t.Helper()
	err := q.client.ZScore(t.Context(), q.getCancelledKey(), id).Err()
	if err == redis.Nil {
		return false
	}
	require.NoError(t, err)
	return true
}

// envID is the envelope id of popped item v.
func envID(t *testing.T, v any) string {
	t.Helper()
	var e struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(v.([]byte), &e))
	return e.ID
}
