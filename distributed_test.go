package redisq

import (
	"context"
	"crypto/sha1"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDistributedQueue(t *testing.T) (*DistributedQueue, func()) {
	redisURL := getTestRedisURL()
	qs := New(redisURL)
	q := qs.NewDistributedQueue(testQueueKey)

	// Clear the queue before test
	ctx := context.Background()
	require.NoError(t, q.Queue.client.Del(ctx, testQueueKey).Err(), "Failed to clear test queue")

	cleanup := func() {
		q.Queue.client.Del(ctx, testQueueKey)
		q.Close()
		qs.Close()
	}

	return q, cleanup
}

func TestNewDistributedQueue(t *testing.T) {
	q, cleanup := setupTestDistributedQueue(t)
	defer cleanup()

	assert.NotNil(t, q.Queue, "Queue should not be nil")
	assert.NotNil(t, q.Notification, "Notification should not be nil")
}

func TestDistributedQueueEnqueueDequeue(t *testing.T) {
	q, cleanup := setupTestDistributedQueue(t)
	defer cleanup()

	notifications := make(chan string, 2)
	q.Subscribe(func(action string) {
		notifications <- action
	})
	assert.Equal(t, 0, q.Len(), "an empty queue marks the consumer idle")

	// Test Enqueue with notification
	assert.True(t, q.Enqueue("test data"))
	select {
	case action := <-notifications:
		assert.Equal(t, "enqueued", action)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for enqueue notification")
	}

	// Test Dequeue with notification
	data, ok := q.Dequeue()
	assert.True(t, ok)
	assert.Equal(t, []byte("test data"), data)

	select {
	case action := <-notifications:
		t.Fatalf("unexpected %q notification after dequeue", action)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDistributedQueueConcurrency(t *testing.T) {
	q, cleanup := setupTestDistributedQueue(t)
	defer cleanup()

	const numGoroutines = 10
	const numOperations = 100

	notifications := make(chan string, numGoroutines*numOperations)
	q.Subscribe(func(action string) {
		notifications <- action
	})
	assert.Equal(t, 0, q.Len())
	before := publishes(t, q.Queue.client)

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Enqueue concurrently
	for range numGoroutines {
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				assert.True(t, q.Enqueue([]byte("test")))
			}
		}()
	}

	wg.Wait()

	// One idle consumer gets one wake for the whole burst, not one per item:
	// once woken it keeps pulling on its own.
	assert.Equal(t, 1, publishes(t, q.Queue.client)-before)
	assert.Equal(t, numGoroutines*numOperations, q.Len())
}

// A job whose consumer never acknowledges it must come back on its own, and
// wake distributed workers, without waiting for a process restart.
func TestDistributedQueueSweeperRequeues(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()

	key := "test_sweeper_queue"
	dq := qs.NewDistributedQueue(key)
	defer dq.Close()
	defer dq.Queue.client.Del(context.Background(), key, dq.getNackedItemKey(), dq.getTimeoutKey())

	enqueued := make(chan struct{}, 10)
	dq.Subscribe(func(action string) {
		if action == "enqueued" {
			enqueued <- struct{}{}
		}
	})

	dq.SetVisibilityTimeout(time.Second)
	dq.Len() // idle
	require.True(t, dq.Enqueue("job"))
	<-enqueued

	_, ok, _ := dq.DequeueWithAckId()
	require.True(t, ok)
	dq.Len() // the consumer's loop finds the queue empty again: idle

	select {
	case <-enqueued:
	case <-time.After(4 * time.Second):
		t.Fatal("sweeper did not requeue the expired job")
	}
	assert.Equal(t, 1, dq.Len())
	assert.Equal(t, 0, dq.GetNackedItemsCount())
}

// cmdCounter counts the commands a client sends, except the background
// sweep and nudge scripts.
type cmdCounter struct {
	mx   sync.Mutex
	n    int
	skip map[string]bool // script hashes
}

func newCmdCounter() *cmdCounter {
	return &cmdCounter{skip: map[string]bool{
		nudge.Hash(): true, listRequeue.Hash(): true, zsetRequeue.Hash(): true,
	}}
}

func (c *cmdCounter) record(cmd redis.Cmder) {
	args := cmd.Args()
	if len(args) > 1 {
		switch cmd.Name() {
		case "evalsha":
			if c.skip[fmt.Sprint(args[1])] {
				return
			}
		case "eval":
			if c.skip[fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprint(args[1]))))] {
				return
			}
		}
	}
	c.mx.Lock()
	c.n++
	c.mx.Unlock()
}

func (c *cmdCounter) count() int {
	c.mx.Lock()
	defer c.mx.Unlock()
	return c.n
}

func (c *cmdCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *cmdCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.record(cmd)
		return next(ctx, cmd)
	}
}

func (c *cmdCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			c.record(cmd)
		}
		return next(ctx, cmds)
	}
}

// An enqueue on a distributed queue is one round trip: the push and the wake
// run in one script.
func TestDistributedEnqueueIsOneRoundTrip(t *testing.T) {
	const n = 100

	t.Run("list", func(t *testing.T) {
		q, cleanup := setupTestDistributedQueue(t)
		defer cleanup()
		q.SetExpiration(time.Minute)
		require.True(t, q.Enqueue("warm-up")) // loads the script

		c := newCmdCounter()
		q.Queue.client.AddHook(c)
		for i := range n {
			require.True(t, q.Enqueue(fmt.Sprint(i)))
		}
		assert.Equal(t, n, c.count())
	})

	t.Run("priority", func(t *testing.T) {
		q, cleanup := setupTestDistributedPriorityQueue(t)
		defer cleanup()
		q.SetExpiration(time.Minute)
		require.True(t, q.Enqueue("warm-up", 1))

		c := newCmdCounter()
		q.Queue.client.AddHook(c)
		for i := range n {
			require.True(t, q.Enqueue(fmt.Sprint(i), i))
		}
		assert.Equal(t, n, c.count())
	})
}

// An enqueue onto a backlog wakes every idle consumer, not just one, so they
// share the backlog at once instead of waiting for the nudge.
func TestEnqueueOntoBacklogWakesAllIdle(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	key := "test_backlog_wake"
	ctx := context.Background()

	var consumers []chan string
	for range 3 {
		c := qs.NewDistributedQueue(key)
		defer c.Close()
		got := make(chan string, 10)
		c.Subscribe(func(action string) { got <- action }) // idle
		consumers = append(consumers, got)
	}
	producer := qs.NewDistributedQueue(key)
	defer producer.Close()
	defer producer.Queue.client.Del(ctx, key, key+":idle")

	require.NoError(t, producer.Queue.client.RPush(ctx, key, "a", "b", "c", "d").Err()) // no wake
	require.True(t, producer.Enqueue("e"))

	for i, got := range consumers {
		select {
		case <-got:
		case <-time.After(300 * time.Millisecond):
			t.Fatalf("consumer %d was not woken", i)
		}
	}
}
