package redisq

import (
	"context"
	"fmt"
	"sort"
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
			p := newDropProxy(t, c.pop)
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
			p := newDropProxy(t, c.pop)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)

			for _, v := range []string{"a", "b", "c", "d", "e", "f", "g"} {
				require.True(t, push(v))
			}
			require.NoError(t, c.pop.Load(ctx, q.client).Err())

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

	items, _, _ = popBatch(t, pq, 5)
	assert.Equal(t, []string{"high", "mid", "low"}, items, "requeued at their own priorities")
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
			p := newDropProxy(t, c.pop)
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
			p := newDropProxy(t, c.pop)
			q, push, cleanup := c.setup(t, p.url())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			q.SetVisibilityTimeout(10 * time.Second)

			for _, v := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
				require.True(t, push(v))
			}
			require.NoError(t, c.pop.Load(ctx, q.client).Err())
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

// A distributed consumer with free slots left joins the idle set when the
// pop empties the queue, even when it reused a smaller failed reservation.
func TestDequeueBatchReusedReservationMarksIdle(t *testing.T) {
	ctx := context.Background()
	p := newDropProxy(t, listAckPop)
	qs := New(p.url())
	defer qs.Close()
	dq := qs.NewDistributedQueue("test_batch_reuse_idle")
	defer dq.Close()
	delKeys(t, dq.Queue.client, dq.Queue.queueKey)
	defer delKeys(t, dq.Queue.client, dq.Queue.queueKey)

	dq.Subscribe(func(string) {})
	require.True(t, dq.Enqueue("a"))
	require.True(t, dq.Enqueue("b"))
	require.NoError(t, listAckPop.Load(ctx, dq.Queue.client).Err())
	require.NoError(t, dq.Queue.client.SRem(ctx, dq.idleKey, dq.id).Err())

	p.drops.Store(4)
	_, _, err := dq.DequeueBatchWithAckIds(make([]any, 2), make([]string, 2))
	require.Error(t, err)

	// Wipe the guard's view of the first run so the reused reservation pops
	// afresh: its pop of 2 is full for the reservation but short for 5 slots.
	require.NoError(t, dq.Queue.client.Del(ctx, dq.getNackedItemKey(), dq.getTimeoutKey()).Err())
	require.True(t, dq.Enqueue("c"))
	require.True(t, dq.Enqueue("d"))
	require.NoError(t, dq.Queue.client.SRem(ctx, dq.idleKey, dq.id).Err())

	items, _, remaining := popBatch(t, dq, 5)
	assert.Equal(t, []string{"c", "d"}, items)
	assert.Equal(t, 0, remaining)
	member, err := dq.Queue.client.SIsMember(ctx, dq.idleKey, dq.id).Result()
	require.NoError(t, err)
	assert.True(t, member, "the consumer still had free slots and the queue is empty")
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
	items, _, _ = popBatch(t, pq, 12)
	assert.Equal(t, want, items)
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
