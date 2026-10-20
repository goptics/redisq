package redisq

import (
	"context"
	"sync"
	"testing"
	"time"

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
	q.Start()

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
		assert.Equal(t, "dequeued", action)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for dequeue notification")
	}
}

func TestDistributedQueueConcurrency(t *testing.T) {
	q, cleanup := setupTestDistributedQueue(t)
	defer cleanup()

	const numGoroutines = 10
	const numOperations = 100

	notifications := make(chan string, numGoroutines*numOperations*2) // *2 for enqueue and dequeue
	q.Subscribe(func(action string) {
		notifications <- action
	})
	q.Start()

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

	// Count notifications
	enqueueCount := 0
	timeout := time.After(5 * time.Second)

	for enqueueCount < numGoroutines*numOperations {
		select {
		case action := <-notifications:
			if action == "enqueued" {
				enqueueCount++
			}
		case <-timeout:
			t.Fatalf("Timeout waiting for notifications. Got %d of %d", enqueueCount, numGoroutines*numOperations)
		}
	}

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
	require.True(t, dq.Enqueue("job"))
	<-enqueued

	_, ok, _ := dq.DequeueWithAckId()
	require.True(t, ok)

	select {
	case <-enqueued:
	case <-time.After(4 * time.Second):
		t.Fatal("sweeper did not requeue the expired job")
	}
	assert.Equal(t, 1, dq.Len())
	assert.Equal(t, 0, dq.GetNackedItemsCount())
}
