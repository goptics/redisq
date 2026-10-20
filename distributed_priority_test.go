package redisq

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDistributedPriorityQueue(t *testing.T) (*DistributedPriorityQueue, func()) {
	redisURL := getTestRedisURL()
	qs := New(redisURL)
	q := qs.NewDistributedPriorityQueue(testQueueKey)

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

func TestDistributedPriorityQueue(t *testing.T) {
	t.Run("basic operations", func(t *testing.T) {
		q, cleanup := setupTestDistributedPriorityQueue(t)
		defer cleanup()

		notifications := make(chan string, 2)
		q.Subscribe(func(action string) {
			notifications <- action
		})

		testCases := []struct {
			name     string
			data     string
			priority int
		}{
			{"high priority", "high", 1},
			{"medium priority", "medium", 2},
			{"low priority", "low", 3},
		}

		// Test Enqueue
		for _, tc := range testCases {
			t.Run("enqueue_"+tc.name, func(t *testing.T) {
				q.markIdle() // as if the consumer's loop went idle again
				assert.True(t, q.Enqueue(tc.data, tc.priority))

				select {
				case action := <-notifications:
					assert.Equal(t, "enqueued", action)
				case <-time.After(time.Second):
					t.Fatal("Timeout waiting for enqueue notification")
				}
			})
		}

		// Test Dequeue (should come out in priority order)
		expectedOrder := []string{"high", "medium", "low"}
		for i, expected := range expectedOrder {
			t.Run("dequeue_priority_"+string(rune('1'+i)), func(t *testing.T) {
				data, ok := q.Dequeue()
				assert.True(t, ok)
				assert.Equal(t, []byte(expected), data)

				select {
				case action := <-notifications:
					t.Fatalf("unexpected %q notification after dequeue", action)
				case <-time.After(100 * time.Millisecond):
				}
			})
		}
	})

	t.Run("concurrent operations", func(t *testing.T) {
		q, cleanup := setupTestDistributedPriorityQueue(t)
		defer cleanup()

		const (
			numGoroutines   = 10
			numOperations   = 100
			totalOperations = numGoroutines * numOperations
		)

		notifications := make(chan string, totalOperations)
		q.Subscribe(func(action string) {
			notifications <- action
		})
		assert.Equal(t, 0, q.Len())
		before := publishes(t, q.Queue.client)

		// Test concurrent enqueue
		t.Run("concurrent_enqueue", func(t *testing.T) {
			var wg sync.WaitGroup
			wg.Add(numGoroutines)

			for i := 0; i < numGoroutines; i++ {
				go func(id int) {
					defer wg.Done()
					for j := 0; j < numOperations; j++ {
						msg := fmt.Sprintf("test-%d-%d", id, j)
						assert.True(t, q.Enqueue(msg, id+1))
					}
				}(i)
			}

			wg.Wait()

			// One wake for the whole burst
			assert.Equal(t, 1, publishes(t, q.Queue.client)-before)
			assert.Equal(t, totalOperations, q.Len())
		})
	})

	t.Run("edge cases", func(t *testing.T) {
		q, cleanup := setupTestDistributedPriorityQueue(t)
		defer cleanup()

		t.Run("empty queue", func(t *testing.T) {
			data, ok := q.Dequeue()
			assert.False(t, ok)
			assert.Nil(t, data)
		})

		t.Run("nil data", func(t *testing.T) {
			assert.False(t, q.Enqueue(nil, 1))
		})
	})
}

// A failed enqueue must not wake workers or count as submitted in varmq.
func TestDistributedPriorityQueueNoNotifyOnFailedEnqueue(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()

	dpq := qs.NewDistributedPriorityQueue("test_failed_enqueue_notify")
	defer dpq.Close()

	got := make(chan string, 1)
	dpq.Subscribe(func(action string) { got <- action })
	dpq.Len() // idle, so a successful enqueue would wake it

	assert.False(t, dpq.Enqueue(42, 1), "unsupported type should fail")

	select {
	case action := <-got:
		t.Fatalf("got %q notification for a failed enqueue", action)
	case <-time.After(300 * time.Millisecond):
	}
}
