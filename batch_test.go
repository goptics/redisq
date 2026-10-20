package redisq

import (
	"context"
	"fmt"
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
// size gets that call's items back; a smaller call doesn't take them, and
// they wait for the visibility timeout.
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
			assert.Equal(t, 7, q.GetNackedItemsCount(), "d, e, f stay pending for the visibility timeout")
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

	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, pq.RequeueNackedItems())
	assert.Equal(t, 0, pq.GetNackedItemsCount())

	items, _, _ = popBatch(t, pq, 5)
	assert.Equal(t, []string{"high", "low"}, items, "requeued at their own priorities")
}
