package redisq

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// popBatch calls DequeueBatchWithAckIds for up to want items and returns
// them as strings.
func popBatch(t *testing.T, q interface {
	DequeueBatchWithAckIds([]any, []string) (int, int, error)
}, want int) (items []string, ackIDs []string, remaining int) {
	t.Helper()
	buf, ids := make([]any, want), make([]string, want)
	n, remaining, err := q.DequeueBatchWithAckIds(buf, ids)
	require.NoError(t, err)
	for i := range n {
		items = append(items, string(buf[i].([]byte)))
	}
	return items, ids[:n], remaining
}

// popAll pops slots at a time until the queue is empty, following remaining
// as a caller does.
func popAll(t *testing.T, q interface {
	DequeueBatchWithAckIds([]any, []string) (int, int, error)
}, slots int) []string {
	t.Helper()
	var all []string
	for {
		items, _, remaining := popBatch(t, q, slots)
		all = append(all, items...)
		if remaining == 0 || len(items) == 0 {
			return all
		}
	}
}

func delKeys(t *testing.T, c *redis.Client, key string) {
	t.Helper()
	require.NoError(t, c.Del(context.Background(), key, key+":nacked", key+":timeouts", key+":prio", key+":seq", key+":idle").Err())
}

func TestDequeueBatchList(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	q := qs.NewQueue("test_batch_list")
	defer q.Close()
	delKeys(t, q.client, q.queueKey)
	defer delKeys(t, q.client, q.queueKey)
	ctx := context.Background()

	for _, v := range []string{"a", "b", "c", "d", "e"} {
		require.True(t, q.Enqueue(v))
	}

	items, ids, remaining := popBatch(t, q, 3)
	assert.Equal(t, []string{"a", "b", "c"}, items, "FIFO")
	assert.Equal(t, 2, remaining)
	assert.Len(t, ids, 3)
	for i, id := range ids {
		pending, err := q.client.HGet(ctx, q.getNackedItemKey(), id).Result()
		require.NoError(t, err)
		assert.Equal(t, items[i], pending)
	}
	n, err := q.client.ZCard(ctx, q.getTimeoutKey()).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 3, n, "every popped item has a deadline")

	items, _, remaining = popBatch(t, q, 10)
	assert.Equal(t, []string{"d", "e"}, items, "a short pop takes what is left")
	assert.Equal(t, 0, remaining)

	items, _, remaining = popBatch(t, q, 10)
	assert.Empty(t, items)
	assert.Equal(t, 0, remaining)

	for _, id := range ids {
		assert.True(t, q.Acknowledge(id))
	}
	assert.Equal(t, 2, q.GetNackedItemsCount())
}

func TestDequeueBatchPriority(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	pq := qs.NewPriorityQueue("test_batch_prio")
	defer pq.Close()
	delKeys(t, pq.client, pq.queueKey)
	defer delKeys(t, pq.client, pq.queueKey)
	ctx := context.Background()

	require.True(t, pq.Enqueue("x", 5))
	require.True(t, pq.Enqueue("a", 1))
	require.True(t, pq.Enqueue("b", 1))
	require.True(t, pq.Enqueue("c", 3))
	// A member written by a version before the sequence prefix.
	require.NoError(t, pq.client.ZAdd(ctx, pq.queueKey, redis.Z{Score: 1, Member: "old"}).Err())

	items, ids, remaining := popBatch(t, pq, 4)
	assert.Equal(t, []string{"old", "a", "b", "c"}, items, "lowest score first, FIFO within a score, legacy members first")
	assert.Equal(t, 1, remaining)

	for i, want := range []string{"1", "1", "1", "3"} {
		score, err := pq.client.HGet(ctx, pq.getPriorityKey(), ids[i]).Result()
		require.NoError(t, err)
		assert.Equal(t, want, score, "the score is kept for a requeue")
		pending, err := pq.client.HGet(ctx, pq.getNackedItemKey(), ids[i]).Result()
		require.NoError(t, err)
		assert.Equal(t, items[i], pending, "pending holds the bare item")
	}
}

// Only a pop that comes up short marks a distributed consumer idle, and only
// a consumer.
func TestDequeueBatchShortPopMarksConsumerIdle(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	ctx := context.Background()

	dq := qs.NewDistributedQueue("test_batch_idle_list")
	defer dq.Close()
	dpq := qs.NewDistributedPriorityQueue("test_batch_idle_prio")
	defer dpq.Close()

	for _, c := range []struct {
		name string
		n    *Notification
		q    interface {
			DequeueBatchWithAckIds([]any, []string) (int, int, error)
		}
		push func(string) bool
	}{
		{"list", dq.Notification, dq, func(s string) bool { return dq.Enqueue(s) }},
		{"priority", dpq.Notification, dpq, func(s string) bool { return dpq.Enqueue(s, 1) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			delKeys(t, c.n.client, c.n.queueKey)
			defer delKeys(t, c.n.client, c.n.queueKey)
			member := func() bool {
				ok, err := c.n.client.SIsMember(ctx, c.n.idleKey, c.n.id).Result()
				require.NoError(t, err)
				return ok
			}

			for _, v := range []string{"a", "b", "c"} {
				require.True(t, c.push(v))
			}
			popBatch(t, c.q, 2)
			assert.False(t, member(), "not a consumer yet")
			popBatch(t, c.q, 5)
			assert.False(t, member(), "a producer never joins the idle set")

			c.n.Subscribe(func(string) {})
			require.NoError(t, c.n.client.SRem(ctx, c.n.idleKey, c.n.id).Err())
			for _, v := range []string{"a", "b", "c"} {
				require.True(t, c.push(v))
			}

			popBatch(t, c.q, 2)
			assert.False(t, member(), "a full pop leaves the consumer busy")
			items, _, _ := popBatch(t, c.q, 5)
			assert.Equal(t, []string{"c"}, items)
			assert.True(t, member(), "a short pop marks the consumer idle")
		})
	}
}

// A batch pop whose reply is lost is re-sent by go-redis; the re-run hands
// back the same items instead of popping the next ones under the same IDs.
func TestDequeueBatchRetryAfterLostReply(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := newDropProxy(t, c.pop, c.popOneLen)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)

			var want []string
			for i := range 40 {
				v := fmt.Sprintf("job-%02d", i)
				require.True(t, push(v))
				if i < 32 {
					want = append(want, v)
				}
			}
			require.NoError(t, c.pop.Load(ctx, q.client).Err())
			require.NoError(t, c.popOneLen.Load(ctx, q.client).Err())

			batcher := interface {
				DequeueBatchWithAckIds([]any, []string) (int, int, error)
			}(q)
			if c.name == "priority" {
				batcher = &PriorityQueue{Queue: q}
			}

			p.drops.Store(1)
			items, _, remaining := popBatch(t, batcher, 32)
			assert.EqualValues(t, 2, p.sends.Load(), "the pop should run, lose its reply and be re-sent once")
			assert.Equal(t, want, items)
			assert.Equal(t, 8, remaining)
			assert.Equal(t, 32, q.GetNackedItemsCount())
		})
	}
}

// After a call whose every attempt lost its reply, the next call of the same
// size gets that call's items back; a smaller call leaves them for the next
// call that is big enough.
func TestDequeueBatchAfterFailedCall(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := newDropProxy(t, c.pop, c.popOneLen)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)

			for _, v := range []string{"a", "b", "c", "d", "e", "f", "g"} {
				require.True(t, push(v))
			}
			require.NoError(t, c.pop.Load(ctx, q.client).Err())
			require.NoError(t, c.popOneLen.Load(ctx, q.client).Err())

			batcher := interface {
				DequeueBatchWithAckIds([]any, []string) (int, int, error)
			}(q)
			if c.name == "priority" {
				batcher = &PriorityQueue{Queue: q}
			}

			p.drops.Store(4) // go-redis's 1 try + 3 retries
			buf, ids := make([]any, 3), make([]string, 3)
			_, _, err := batcher.DequeueBatchWithAckIds(buf, ids)
			require.Error(t, err)
			assert.Equal(t, 3, q.GetNackedItemsCount(), "the first attempt popped a, b, c")

			items, _, _ := popBatch(t, batcher, 3)
			assert.Equal(t, []string{"a", "b", "c"}, items)

			p.drops.Store(4)
			_, _, err = batcher.DequeueBatchWithAckIds(buf, ids)
			require.Error(t, err) // d, e, f popped, reply lost

			items, _, _ = popBatch(t, batcher, 1)
			assert.Equal(t, []string{"g"}, items, "a smaller call does not take the failed call's items")
			assert.Equal(t, 7, q.GetNackedItemsCount())

			items, _, _ = popBatch(t, batcher, 3)
			assert.Equal(t, []string{"d", "e", "f"}, items, "the next call big enough gets them back")
		})
	}
}

// Concurrent batch pops on several Queue objects never hand out an item
// twice or lose one.
func TestDequeueBatchConcurrent(t *testing.T) {
	const total, workers, batch = 600, 8, 7
	qs := New(getTestRedisURL())
	defer qs.Close()
	key := "test_batch_concurrent"
	q0 := qs.NewQueue(key)
	defer q0.Close()
	delKeys(t, q0.client, key)
	defer delKeys(t, q0.client, key)

	for i := range total {
		require.True(t, q0.Enqueue(fmt.Sprint(i)))
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range workers {
		q := qs.NewQueue(key)
		defer q.Close()
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf, ids := make([]any, batch), make([]string, batch)
			for {
				n, _, err := q.DequeueBatchWithAckIds(buf, ids)
				if !assert.NoError(t, err) || n == 0 {
					return
				}
				mu.Lock()
				for i := range n {
					seen[string(buf[i].([]byte))]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Len(t, seen, total)
	for item, count := range seen {
		assert.Equal(t, 1, count, "item %s handed out more than once", item)
	}
	assert.Equal(t, total, q0.GetNackedItemsCount())
}

// A batch shares one deadline, so the sweeper's requeue brings back all of it.
func TestDequeueBatchRequeue(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	pq := qs.NewPriorityQueue("test_batch_requeue")
	defer pq.Close()
	delKeys(t, pq.client, pq.queueKey)
	defer delKeys(t, pq.client, pq.queueKey)

	pq.SetVisibilityTimeout(100 * time.Millisecond)
	require.True(t, pq.Enqueue("low", 7))
	require.True(t, pq.Enqueue("high", 2))
	items, _, _ := popBatch(t, pq, 5)
	require.Equal(t, []string{"high", "low"}, items)

	require.True(t, pq.Enqueue("mid", 5))
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, pq.RequeueNackedItems())
	assert.Equal(t, 0, pq.GetNackedItemsCount())

	assert.Equal(t, []string{"high", "mid", "low"}, popAll(t, pq, 5), "requeued at their own priorities")
}

type batcher interface {
	DequeueBatchWithAckIds([]any, []string) (int, int, error)
}

// allItems returns every item in the queue or pending, sorted.
func allItems(t *testing.T, c *redis.Client, key string, priority bool) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	if priority {
		ms, err := c.ZRange(ctx, key, 0, -1).Result()
		require.NoError(t, err)
		for _, m := range ms {
			out = append(out, string(itemOf(m)))
		}
	} else {
		ms, err := c.LRange(ctx, key, 0, -1).Result()
		require.NoError(t, err)
		out = append(out, ms...)
	}
	vals, err := c.HVals(ctx, key+":nacked").Result()
	require.NoError(t, err)
	out = append(out, vals...)
	sort.Strings(out)
	return out
}

func batcherFor(c retryCase, q *Queue) batcher {
	if c.name == "priority" {
		return &PriorityQueue{Queue: q}
	}
	return q
}

// A copy of a batch pop that reaches Redis late, after the caller acked part
// of the batch, must not pop new items over the IDs still pending: that lost
// the new items when the rest of the batch was acked.
func TestDequeueBatchLateDuplicateAfterPartialAck(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			q, push, cleanup := c.setup(t, getTestRedisURL())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			for _, v := range []string{"x1", "x2", "x3", "y1", "y2", "y3"} {
				require.True(t, push(v))
			}

			b := batcherFor(c, q)
			resv := fmt.Sprintf("%020d", q.ackSeq.Load()+1) // the reservation the call will get
			items, ids, _ := popBatch(t, b, 3)
			require.Equal(t, []string{"x1", "x2", "x3"}, items)
			require.True(t, q.Acknowledge(ids[0]))

			keys := []string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey(), q.getIdleKey(), q.getPriorityKey()}
			deadline := unixSeconds(time.Now().Add(time.Minute))
			require.NoError(t, c.pop.Run(ctx, q.client, keys, 3, deadline, q.ackPrefix, resv, "", 3).Err())

			pending, err := q.client.HMGet(ctx, q.getNackedItemKey(), ids[1], ids[2]).Result()
			require.NoError(t, err)
			assert.Equal(t, []any{"x2", "x3"}, pending, "the late copy must not overwrite live pending entries")

			require.True(t, q.Acknowledge(ids[1]))
			require.True(t, q.Acknowledge(ids[2]))
			assert.Equal(t, []string{"y1", "y2", "y3"}, allItems(t, q.client, q.queueKey, c.name == "priority"))
		})
	}
}

// A short pop whose reply is lost, with new items queued before go-redis
// re-sends it: the re-send hands back exactly the first run's items.
func TestDequeueBatchShortPopRetry(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := newDropProxy(t, c.pop, c.popOneLen)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			direct := New(getTestRedisURL()).(*queues)
			defer direct.Close()

			var first, all []string
			for i := range 10 {
				v := fmt.Sprintf("a%d", i)
				require.True(t, push(v))
				first = append(first, v)
				all = append(all, v)
			}
			late := []string{"n0", "n1", "n2", "n3", "n4"}
			all = append(all, late...)
			p.onDrop = func() { // between the first run and the re-send
				for _, v := range late {
					if c.name == "priority" {
						assert.NoError(t, zsetPush.Run(ctx, direct.client, []string{q.queueKey, q.queueKey + ":seq"}, 1, v, 0).Err())
					} else {
						assert.NoError(t, direct.client.RPush(ctx, q.queueKey, v).Err())
					}
				}
			}
			require.NoError(t, c.pop.Load(ctx, q.client).Err())
			require.NoError(t, c.popOneLen.Load(ctx, q.client).Err())

			p.drops.Store(1)
			items, _, remaining := popBatch(t, batcherFor(c, q), 32)
			assert.EqualValues(t, 2, p.sends.Load())
			assert.Equal(t, first, items, "the re-send hands back the first run's short pop")
			assert.Equal(t, 5, remaining, "and pops nothing more")
			sort.Strings(all)
			assert.Equal(t, all, allItems(t, q.client, q.queueKey, c.name == "priority"))
		})
	}
}

// After a failed call of 3, the next call with more free slots gets the
// failed call's items back now, with a fresh visibility deadline.
func TestDequeueBatchLargerCallAfterFailedCall(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := newDropProxy(t, c.pop, c.popOneLen)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			q.SetVisibilityTimeout(10 * time.Second)

			for _, v := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
				require.True(t, push(v))
			}
			require.NoError(t, c.pop.Load(ctx, q.client).Err())
			require.NoError(t, c.popOneLen.Load(ctx, q.client).Err())
			b := batcherFor(c, q)

			p.drops.Store(4)
			_, _, err := b.DequeueBatchWithAckIds(make([]any, 3), make([]string, 3))
			require.Error(t, err)
			time.Sleep(1500 * time.Millisecond)

			items, ids, _ := popBatch(t, b, 5)
			assert.Equal(t, []string{"a", "b", "c"}, items, "a larger call gets the failed call's items back")
			minDeadline := unixSeconds(time.Now().Add(10*time.Second - 300*time.Millisecond))
			for i, id := range ids {
				v, err := q.client.HGet(ctx, q.getNackedItemKey(), id).Result()
				require.NoError(t, err)
				assert.Equal(t, items[i], v)
				d, err := q.client.ZScore(ctx, q.getTimeoutKey(), id).Result()
				require.NoError(t, err)
				assert.GreaterOrEqual(t, d, minDeadline, "the handed-back item gets a full visibility timeout")
			}
			assert.Equal(t, 3, q.GetNackedItemsCount())
		})
	}
}

// A distributed consumer joins the idle set when a call leaves the queue
// empty with fewer items than its free slots, also when the call reused a
// smaller failed reservation, and only then.
func TestDequeueBatchReusedReservationMarksIdle(t *testing.T) {
	ctx := context.Background()
	p := newDropProxy(t, listAckPop, listAckPopOneLen)
	qs := New(p.url())
	defer qs.Close()
	dq := qs.NewDistributedQueue("test_batch_reuse_idle")
	defer dq.Close()
	delKeys(t, dq.Queue.client, dq.Queue.queueKey)
	defer delKeys(t, dq.Queue.client, dq.Queue.queueKey)
	dq.Subscribe(func(string) {})
	require.NoError(t, listAckPop.Load(ctx, dq.Queue.client).Err())
	require.NoError(t, listAckPopOneLen.Load(ctx, dq.Queue.client).Err())

	member := func() bool {
		ok, err := dq.Queue.client.SIsMember(ctx, dq.idleKey, dq.id).Result()
		require.NoError(t, err)
		return ok
	}
	// failPop queues vs and loses every reply of a 2-slot pop, which takes
	// the first two; wipe drops its pending items so the next call's reused
	// reservation pops afresh instead of handing them back.
	failPop := func(wipe bool, vs ...string) {
		for _, v := range vs {
			require.True(t, dq.Enqueue(v))
		}
		dq.Queue.popLen.Store(maxPop) // the failing call reserves both its slots
		p.drops.Store(4)
		_, _, err := dq.DequeueBatchWithAckIds(make([]any, 2), make([]string, 2))
		require.Error(t, err)
		if wipe {
			require.NoError(t, dq.Queue.client.Del(ctx, dq.getNackedItemKey(), dq.getTimeoutKey()).Err())
		}
		require.NoError(t, dq.Queue.client.SRem(ctx, dq.idleKey, dq.id).Err())
	}

	failPop(false, "a", "b")
	items, _, remaining := popBatch(t, dq, 5)
	assert.Equal(t, []string{"a", "b"}, items, "handed back")
	assert.Equal(t, 0, remaining)
	assert.True(t, member(), "a hand-back short of the free slots that leaves the queue empty")

	failPop(true, "c", "d")
	require.True(t, dq.Enqueue("e"))
	require.NoError(t, dq.Queue.client.SRem(ctx, dq.idleKey, dq.id).Err())
	items, _, remaining = popBatch(t, dq, 5)
	assert.Equal(t, []string{"e"}, items)
	assert.Equal(t, 0, remaining)
	assert.True(t, member(), "a fresh pop short of the free slots that leaves the queue empty")

	failPop(true, "f", "g")
	for _, v := range []string{"h", "i", "j"} {
		require.True(t, dq.Enqueue(v))
	}
	require.NoError(t, dq.Queue.client.SRem(ctx, dq.idleKey, dq.id).Err())
	items, _, remaining = popBatch(t, dq, 5)
	assert.Equal(t, []string{"h", "i"}, items, "the reused reservation takes 2")
	assert.Equal(t, 1, remaining)
	assert.False(t, member(), "items still wait")
}

// A buffer bigger than maxPop that empties the queue still has free slots,
// so the consumer joins the idle set.
func TestDequeueBatchMaxPopMarksIdle(t *testing.T) {
	ctx := context.Background()
	qs := New(getTestRedisURL())
	defer qs.Close()
	dq := qs.NewDistributedQueue("test_batch_maxpop_idle")
	defer dq.Close()
	delKeys(t, dq.Queue.client, dq.Queue.queueKey)
	defer delKeys(t, dq.Queue.client, dq.Queue.queueKey)
	dq.Subscribe(func(string) {})

	vs := make([]any, maxPop)
	for i := range vs {
		vs[i] = fmt.Sprintf("j%04d", i)
	}
	require.NoError(t, dq.Queue.client.RPush(ctx, dq.Queue.queueKey, vs...).Err())
	require.NoError(t, dq.Queue.client.SRem(ctx, dq.idleKey, dq.id).Err())

	n, remaining, err := dq.DequeueBatchWithAckIds(make([]any, 1500), make([]string, 1500))
	require.NoError(t, err)
	assert.Equal(t, maxPop, n)
	assert.Equal(t, 0, remaining)
	member, err := dq.Queue.client.SIsMember(ctx, dq.idleKey, dq.id).Result()
	require.NoError(t, err)
	assert.True(t, member, "1500 slots, 1000 items, queue empty")
}

// A failed single pop's item comes back from the next batch call, and a
// failed batch's from the next single pop: both share reservations and the
// ack ID format.
func TestFailedPopHandsBackAcrossKinds(t *testing.T) {
	for _, c := range retryCases {
		c.run(t, 4, func(t *testing.T, q *Queue, p *dropProxy, dequeue func() (string, bool, string), size func() int) {
			_, ok, _ := dequeue()
			require.False(t, ok)
			items, _, remaining := popBatch(t, batcherFor(c, q), 3)
			assert.Equal(t, []string{"a"}, items, "single pop failed, batch gets its item back")
			assert.Equal(t, 1, remaining)
			assert.Equal(t, 1, q.GetNackedItemsCount())
		})
	}
	for _, c := range retryCases {
		t.Run(c.name+"/batch-then-single", func(t *testing.T) {
			p := newDropProxy(t, c.pop, c.popOneLen)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			require.True(t, push("a"))
			require.True(t, push("b"))
			require.NoError(t, c.pop.Load(context.Background(), q.client).Err())
			require.NoError(t, c.popOneLen.Load(context.Background(), q.client).Err())
			b := batcherFor(c, q)

			p.drops.Store(4)
			_, _, err := b.DequeueBatchWithAckIds(make([]any, 1), make([]string, 1))
			require.Error(t, err)
			item, ok, _ := b.(interface{ DequeueWithAckId() (any, bool, string) }).DequeueWithAckId()
			require.True(t, ok)
			assert.Equal(t, "a", string(item.([]byte)), "batch failed, single pop gets its item back")
			assert.Equal(t, 1, q.GetNackedItemsCount())
		})
	}
}

// A single pop that hands back a failed call's item gives it a fresh
// visibility deadline.
func TestPopHandBackRaisesDeadline(t *testing.T) {
	for _, c := range retryCases {
		c.run(t, 4, func(t *testing.T, q *Queue, p *dropProxy, dequeue func() (string, bool, string), size func() int) {
			q.SetVisibilityTimeout(10 * time.Second)
			_, ok, _ := dequeue()
			require.False(t, ok)
			time.Sleep(1500 * time.Millisecond)

			item, ok, ackID := dequeue()
			require.True(t, ok)
			assert.Equal(t, "a", item)
			d, err := q.client.ZScore(context.Background(), q.getTimeoutKey(), ackID).Result()
			require.NoError(t, err)
			assert.GreaterOrEqual(t, d, unixSeconds(time.Now().Add(10*time.Second-300*time.Millisecond)))
		})
	}
}

// A buffer bigger than maxPop takes maxPop items and loses none.
func TestDequeueBatchMaxPop(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			q, _, cleanup := c.setup(t, getTestRedisURL())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)

			const total = 2500
			for i := range total {
				v := fmt.Sprintf("j%05d", i)
				if c.name == "priority" {
					require.NoError(t, zsetPush.Run(ctx, q.client, []string{q.queueKey, q.queueKey + ":seq"}, 1, v, 0).Err())
				} else {
					require.NoError(t, q.client.RPush(ctx, q.queueKey, v).Err())
				}
			}

			n, remaining, err := batcherFor(c, q).DequeueBatchWithAckIds(make([]any, total), make([]string, total))
			require.NoError(t, err)
			assert.Equal(t, maxPop, n)
			assert.Equal(t, total-maxPop, remaining)
			assert.Len(t, allItems(t, q.client, q.queueKey, c.name == "priority"), total)
		})
	}
}

// Goroutines sharing one Queue object never get the same item or ack ID.
func TestDequeueBatchConcurrentSharedObject(t *testing.T) {
	const total, workers, batch = 1500, 8, 7
	qs := New(getTestRedisURL())
	defer qs.Close()
	q := qs.NewQueue("test_batch_shared")
	defer q.Close()
	delKeys(t, q.client, q.queueKey)
	defer delKeys(t, q.client, q.queueKey)
	for i := range total {
		require.True(t, q.Enqueue(fmt.Sprint(i)))
	}

	var mu sync.Mutex
	seen, idSeen := map[string]int{}, map[string]int{}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf, ids := make([]any, batch), make([]string, batch)
			for {
				n, _, err := q.DequeueBatchWithAckIds(buf, ids)
				if !assert.NoError(t, err) || n == 0 {
					return
				}
				mu.Lock()
				for i := range n {
					seen[string(buf[i].([]byte))]++
					idSeen[ids[i]]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Len(t, seen, total)
	assert.Len(t, idSeen, total, "every ack ID is unique")
	for item, count := range seen {
		assert.Equal(t, 1, count, "item %s handed out more than once", item)
	}
	assert.Equal(t, total, q.GetNackedItemsCount())
}

// A requeued batch keeps its order past 9 items: ack IDs sort in pop order.
func TestDequeueBatchRequeueKeepsOrder(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	pq := qs.NewPriorityQueue("test_batch_requeue_order")
	defer pq.Close()
	delKeys(t, pq.client, pq.queueKey)
	defer delKeys(t, pq.client, pq.queueKey)

	var want []string
	for i := range 12 {
		v := fmt.Sprintf("job-%02d", i)
		require.True(t, pq.Enqueue(v, 1))
		want = append(want, v)
	}
	pq.SetVisibilityTimeout(100 * time.Millisecond)
	items, _, _ := popBatch(t, pq, 12)
	require.Equal(t, want, items)

	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, pq.RequeueNackedItems())
	assert.Equal(t, want, popAll(t, pq, 12))
}

// IDs stay exact and distinct with a huge reservation counter: no number goes
// through Lua arithmetic.
func TestDequeueBatchLargeCounter(t *testing.T) {
	ctx := context.Background()
	qs := New(getTestRedisURL())
	defer qs.Close()
	q := qs.NewQueue("test_batch_large_counter")
	defer q.Close()
	delKeys(t, q.client, q.queueKey)
	defer delKeys(t, q.client, q.queueKey)

	q.ackSeq.Store(1<<62 + 12345)
	for _, v := range []string{"a", "b", "c"} {
		require.True(t, q.Enqueue(v))
	}
	items, ids, _ := popBatch(t, q, 3)
	require.Equal(t, []string{"a", "b", "c"}, items)
	for i, id := range ids {
		v, err := q.client.HGet(ctx, q.getNackedItemKey(), id).Result()
		require.NoError(t, err)
		assert.Equal(t, items[i], v)
	}
}

// A call sizes itself by the previous ones: a fresh object fills the call,
// a trickle pops one item per call with the lean script, a burst after a
// trickle takes two calls, a burst like the last one takes one, and an empty
// call keeps the estimate.
func TestDequeueBatchSizesByRun(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := newDropProxy(t, c.popOneLen) // counts lean calls; drops nothing
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			require.NoError(t, c.popOneLen.Load(ctx, q.client).Err())
			b := batcherFor(c, q)
			pushAll := func(vs ...string) {
				for _, v := range vs {
					require.True(t, push(v))
				}
			}
			pop := func(want []string, remaining int, lean int32, msg string) []string {
				t.Helper()
				before := p.sends.Load()
				items, ids, rem := popBatch(t, b, 10)
				assert.Equal(t, want, items, msg)
				assert.Equal(t, remaining, rem, msg)
				assert.Equal(t, lean, p.sends.Load()-before, msg+": lean calls")
				return ids
			}

			pushAll("a", "b", "c")
			pop([]string{"a", "b", "c"}, 0, 0, "a fresh object fills the call")
			pushAll("d")
			pop([]string{"d"}, 0, 0, "sized by the last run of 3")
			pushAll("e")
			ids := pop([]string{"e"}, 0, 1, "a trickle: one item, lean")
			require.Len(t, ids, 1)
			assert.True(t, strings.HasSuffix(ids[0], ":0001"))
			v, err := q.client.HGet(ctx, q.getNackedItemKey(), ids[0]).Result()
			require.NoError(t, err)
			assert.Equal(t, "e", v)
			_, err = q.client.ZScore(ctx, q.getTimeoutKey(), ids[0]).Result()
			assert.NoError(t, err, "the item has a deadline")
			if c.name == "priority" {
				score, err := q.client.HGet(ctx, q.getPriorityKey(), ids[0]).Result()
				require.NoError(t, err)
				assert.Equal(t, "1", score)
			}

			pushAll("f", "g", "h")
			pop([]string{"f"}, 2, 1, "a burst after a trickle: one item first")
			pop([]string{"g", "h"}, 0, 0, "then what remained")
			pop(nil, 0, 0, "an empty call")
			pushAll("i", "j", "k")
			pop([]string{"i", "j", "k"}, 0, 0, "a burst like the last run, in one call, despite the empty call")
			assert.Equal(t, 11, q.GetNackedItemsCount(), "every popped item is pending")
		})
	}
}

// The lean one-item script's rules, run directly with a consumer ID no
// Notification owns, so the nudge loop can't touch the idle set: an empty
// pop joins it, a pop leaving an item or filling its only slot does not, a
// caller that is not a consumer never does, and a hand-back that empties the
// queue with slots to spare joins it and gets a fresh deadline.
func TestDequeueBatchLeanScriptRules(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			q, push, cleanup := c.setup(t, getTestRedisURL())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			keys := []string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey(), q.getIdleKey(), q.getPriorityKey()}
			const cons = "synthetic-consumer"
			member := func() bool {
				ok, err := q.client.SIsMember(ctx, q.getIdleKey(), cons).Result()
				require.NoError(t, err)
				return ok
			}
			resv := 0
			run := func(consumer string, slots int, id string) (int64, []any) {
				t.Helper()
				if id == "" {
					resv++
					id = q.ackIDOf(fmt.Sprintf("%020d", resv), 1)
				}
				r, err := c.popOneLen.Run(ctx, q.client, keys, unixSeconds(time.Now().Add(time.Minute)), id, consumer, slots).Slice()
				require.NoError(t, err)
				return r[0].(int64), r[1].([]any)
			}

			n, items := run("", 5, "")
			assert.EqualValues(t, 0, n)
			assert.Empty(t, items)
			assert.Zero(t, q.client.SCard(ctx, q.getIdleKey()).Val(), "not a consumer")

			n, items = run(cons, 5, "")
			assert.EqualValues(t, 0, n)
			assert.Empty(t, items)
			assert.True(t, member(), "empty pop")

			require.NoError(t, q.client.SRem(ctx, q.getIdleKey(), cons).Err())
			require.True(t, push("a"))
			require.True(t, push("b"))
			n, items = run(cons, 5, "")
			assert.EqualValues(t, 1, n)
			assert.Equal(t, []any{"a"}, items)
			assert.False(t, member(), "an item still waits")

			n, items = run(cons, 1, "")
			assert.EqualValues(t, 0, n)
			assert.Equal(t, []any{"b"}, items)
			assert.False(t, member(), "no slot to spare")

			id := q.ackIDOf(fmt.Sprintf("%020d", 99), 1)
			require.NoError(t, q.client.HSet(ctx, q.getNackedItemKey(), id, "held").Err())
			old := unixSeconds(time.Now().Add(time.Second))
			require.NoError(t, q.client.ZAdd(ctx, q.getTimeoutKey(), redis.Z{Score: old, Member: id}).Err())
			n, items = run(cons, 5, id)
			assert.EqualValues(t, 0, n)
			assert.Equal(t, []any{"held"}, items)
			assert.True(t, member(), "a hand-back short of the slots that empties the queue")
			d, err := q.client.ZScore(ctx, q.getTimeoutKey(), id).Result()
			require.NoError(t, err)
			assert.Greater(t, d, old+30, "the hand-back raises the deadline")
		})
	}
}

// A single pop leaves a failed batch call's kept reservation alone: the next
// batch call with room for it gets its items back.
func TestDequeueBatchSinglePopLeavesKeptBatch(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := newDropProxy(t, c.pop, c.popOneLen)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			for _, v := range []string{"a", "b", "c"} {
				require.True(t, push(v))
			}
			require.NoError(t, c.pop.Load(ctx, q.client).Err())
			b := batcherFor(c, q)
			p.drops.Store(4)
			_, _, err := b.DequeueBatchWithAckIds(make([]any, 2), make([]string, 2))
			require.Error(t, err) // a, b pending under the kept reservation

			item, ok, _ := b.(interface{ DequeueWithAckId() (any, bool, string) }).DequeueWithAckId()
			require.True(t, ok)
			assert.Equal(t, "c", string(item.([]byte)))
			items, _, _ := popBatch(t, b, 2)
			assert.Equal(t, []string{"a", "b"}, items)
		})
	}
}

// A lean one-item batch pop whose reply is lost is re-sent and hands back the
// same item.
func TestDequeueBatchOneItemRetry(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := newDropProxy(t, c.popOneLen)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			require.NoError(t, c.popOneLen.Load(ctx, q.client).Err())
			require.True(t, push("a"))
			require.True(t, push("b"))
			q.popLen.Store(0) // the next call takes one item

			p.drops.Store(1)
			items, _, remaining := popBatch(t, batcherFor(c, q), 5)
			assert.EqualValues(t, 2, p.sends.Load(), "run, reply lost, re-sent")
			assert.Equal(t, []string{"a"}, items)
			assert.Equal(t, 1, remaining)
			assert.Equal(t, 1, q.GetNackedItemsCount())
		})
	}
}

// On the one-item path, a consumer joins the idle set when the pop empties
// the queue with slots to spare, and not when its only slot is now taken.
func TestDequeueBatchOneItemIdleRule(t *testing.T) {
	ctx := context.Background()
	qs := New(getTestRedisURL())
	defer qs.Close()
	dq := qs.NewDistributedQueue("test_batch_one_idle")
	defer dq.Close()
	delKeys(t, dq.Queue.client, dq.Queue.queueKey)
	defer delKeys(t, dq.Queue.client, dq.Queue.queueKey)
	dq.Subscribe(func(string) {})
	member := func() bool {
		ok, err := dq.Queue.client.SIsMember(ctx, dq.idleKey, dq.id).Result()
		require.NoError(t, err)
		return ok
	}

	for _, slots := range []int{1, 5} {
		require.True(t, dq.Enqueue("x"))
		require.NoError(t, dq.Queue.client.SRem(ctx, dq.idleKey, dq.id).Err())
		dq.Queue.popLen.Store(0)
		items, _, remaining := popBatch(t, dq, slots)
		require.Equal(t, []string{"x"}, items)
		require.Equal(t, 0, remaining)
		assert.Equal(t, slots > 1, member(), "slots %d", slots)
	}
}

// A failed call's reservation is reused by any later call with room for it,
// even one that sizes itself to a single item because the queue looked
// empty: its items come back at once instead of waiting for the timeout.
func TestDequeueBatchReuseAfterSmallGuess(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := newDropProxy(t, c.pop, c.popOneLen)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			require.NoError(t, c.pop.Load(ctx, q.client).Err())
			require.NoError(t, c.popOneLen.Load(ctx, q.client).Err())
			for _, v := range []string{"a", "b", "c", "d"} {
				require.True(t, push(v))
			}
			b := batcherFor(c, q)

			p.drops.Store(4)
			_, _, err := b.DequeueBatchWithAckIds(make([]any, 2), make([]string, 2))
			require.Error(t, err) // a, b popped, every reply lost

			q.popLen.Store(0) // the next call would size itself to one item
			items, _, remaining := popBatch(t, b, 5)
			assert.Equal(t, []string{"a", "b"}, items)
			assert.Equal(t, 2, remaining)
		})
	}
}
