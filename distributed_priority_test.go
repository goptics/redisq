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
		q.Start()

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
					assert.Equal(t, "dequeued", action)
				case <-time.After(time.Second):
					t.Fatal("Timeout waiting for dequeue notification")
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

		notifications := make(chan string, totalOperations*2) // *2 for enqueue and dequeue
		q.Subscribe(func(action string) {
			notifications <- action
		})
		q.Start()

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

			// Verify notifications
			enqueueCount := 0
			timeout := time.After(5 * time.Second)

			for enqueueCount < totalOperations {
				select {
				case action := <-notifications:
					if action == "enqueued" {
						enqueueCount++
					}
				case <-timeout:
					t.Fatalf("Timeout waiting for notifications. Got %d of %d", enqueueCount, totalOperations)
				}
			}

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
