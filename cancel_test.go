package redisq

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/goptics/varmq/contract"
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
	assert.False(t, q.client.SIsMember(ctx, q.getCancelledKey(), id).Val(), "mark gone")
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
		assert.Zero(t, c.cons.client.Exists(t.Context(), c.cons.getCancelledKey()).Val(), "last SREM drops the set")
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
		assert.False(t, c.cons.client.SIsMember(ctx, c.cons.getCancelledKey(), "gone").Val(), "an ended job is not marked")

		require.NoError(t, c.prod.CancelJob("nobody"))
		assert.True(t, c.cons.client.SIsMember(ctx, c.cons.getCancelledKey(), "nobody").Val())
		ttl := c.cons.client.PTTL(ctx, c.cons.getCancelledKey()).Val()
		assert.Greater(t, ttl, 23*time.Hour)

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
		assert.False(t, c.cons.client.SIsMember(ctx, c.cons.getCancelledKey(), "r").Val())
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
			assert.False(t, c.cons.client.SIsMember(ctx, c.cons.getCancelledKey(), "m").Val())
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
		assert.False(t, c.cons.client.SIsMember(ctx, c.cons.getCancelledKey(), "n").Val())
		v, ok, _ := ackPopOf(c.cons)
		require.True(t, ok)
		assert.Equal(t, "n", envID(t, v))

		require.NoError(t, c.prod.CancelJob("p"))
		require.True(t, c.pushOnce(envelope("p", `1`), "p"))
		require.NoError(t, c.prod.CancelJob("p"))
		require.True(t, c.pushOnce(envelope("p", `2`), "p")) // dedupe hit: mark stays
		assert.True(t, c.cons.client.SIsMember(ctx, c.cons.getCancelledKey(), "p").Val())

		c.prod.Purge()
		assert.Zero(t, c.cons.client.Exists(ctx, c.cons.getCancelledKey()).Val())
	})
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
