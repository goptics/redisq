package redisq

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testQueueKey = "test_queue"

func getTestRedisURL() string {
	// Try to load .env file, but don't panic if it doesn't exist
	_ = godotenv.Load()

	// Get REDIS_URL from environment
	if url := os.Getenv("REDIS_URL"); url != "" {
		return url
	}

	// Default to localhost:6379 if not set
	return "redis://localhost:6379"
}

func setupTestQueue(t *testing.T) (*Queue, func()) {
	redisURL := getTestRedisURL()
	qs := New(redisURL)
	q := qs.NewQueue(testQueueKey)

	// Ping Redis to ensure it's available
	ctx := context.Background()
	_, err := q.client.Ping(ctx).Result()
	if err != nil {
		t.Skip("Redis server is not available, skipping test: " + err.Error())
		return nil, func() {}
	}

	// Clear the queue before test
	require.NoError(t, q.client.Del(ctx, testQueueKey).Err(), "Failed to clear test queue")
	require.NoError(t, q.client.Del(ctx, q.getNackedItemKey()).Err(), "Failed to clear nacked queue")

	cleanup := func() {
		q.client.Del(ctx, testQueueKey)
		q.client.Del(ctx, q.getNackedItemKey())
		q.Close()
		qs.Close()
	}

	return q, cleanup
}

func TestNewQueue(t *testing.T) {
	redisURL := getTestRedisURL()
	qs := New(redisURL)
	q := qs.NewQueue(testQueueKey)
	defer qs.Close()
	defer q.Close()

	assert.Equal(t, testQueueKey, q.queueKey, "Queue key should match")
	assert.NotNil(t, q.client, "Redis client should not be nil")
}

func TestEnqueueDequeue(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	tests := []struct {
		name     string
		input    any
		wantOk   bool
		wantData []byte
	}{
		{
			name:     "enqueue bytes",
			input:    []byte("test data"),
			wantOk:   true,
			wantData: []byte("test data"),
		},
		{
			name:     "enqueue string",
			input:    "test string",
			wantOk:   true,
			wantData: []byte("test string"),
		},
		{
			name:     "enqueue invalid type",
			input:    123,
			wantOk:   false,
			wantData: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test Enqueue
			ok := q.Enqueue(tt.input)
			assert.Equal(t, tt.wantOk, ok)

			if !tt.wantOk {
				return
			}

			// Test Dequeue
			data, ok := q.Dequeue()
			assert.True(t, ok, "Dequeue should succeed")

			bytes, ok := data.([]byte)
			assert.True(t, ok, "Dequeued data should be []byte")
			assert.Equal(t, string(tt.wantData), string(bytes))
		})
	}
}

func TestQueueLength(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Test empty queue
	assert.Zero(t, q.Len(), "Queue should be empty")

	// Add items
	testData := []string{"item1", "item2", "item3"}
	for _, item := range testData {
		assert.True(t, q.Enqueue(item), "Enqueue should succeed")
	}

	assert.Equal(t, len(testData), q.Len(), "Queue length should match")
}

func TestQueueExpiration(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	expiration := time.Second
	q.SetExpiration(expiration)

	assert.True(t, q.Enqueue("test"), "Enqueue should succeed")

	time.Sleep(expiration + 100*time.Millisecond)
	assert.Zero(t, q.Len(), "Queue should be empty after expiration")
}

func TestQueueValues(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	testData := []string{"item1", "item2", "item3"}

	for _, item := range testData {
		assert.True(t, q.Enqueue(item), "Enqueue should succeed")
	}

	values := q.Values()
	assert.Len(t, values, len(testData), "Should have correct number of values")

	for i, value := range values {
		bytes, ok := value.([]byte)
		assert.True(t, ok, "Value should be []byte")
		assert.Equal(t, testData[i], string(bytes))
	}
}

func TestQueueConcurrency(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	const numGoroutines = 10
	const numOperations = 100

	done := make(chan bool)

	for i := range numGoroutines {
		go func(id int) {
			for j := 0; j < numOperations; j++ {
				assert.True(t, q.Enqueue([]byte("test")))
			}
			done <- true
		}(i)
	}

	for range numGoroutines {
		<-done
	}

	expectedLength := numGoroutines * numOperations
	assert.Equal(t, expectedLength, q.Len(), "Queue length should match total operations")
}

func TestQueueClose(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	assert.NoError(t, q.Close(), "Close should succeed")

	assert.False(t, q.Enqueue("test"), "Enqueue should fail after close")
	_, ok := q.Dequeue()
	assert.False(t, ok, "Dequeue should fail after close")

	assert.Error(t, q.PrepareForFutureAck("test-ack", "test"), "PrepareForFutureAck should fail after close")
	assert.False(t, q.Acknowledge("test-ack"), "Acknowledge should fail after close")
}

func TestPrepareForFutureAck(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Test with string item
	err := q.PrepareForFutureAck("ack-1", "test-item")
	assert.NoError(t, err, "PrepareForFutureAck should succeed with string")

	// Test with byte array
	err = q.PrepareForFutureAck("ack-2", []byte("test-bytes"))
	assert.NoError(t, err, "PrepareForFutureAck should succeed with bytes")

	// Verify items are in the nacked items list
	count := q.GetNackedItemsCount()
	assert.Equal(t, 2, count, "Should have 2 items in nacked list")
}

func TestAcknowledge(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Add an item to acknowledge
	err := q.PrepareForFutureAck("ack-id", "test-item")
	assert.NoError(t, err, "PrepareForFutureAck should succeed")

	// Verify item is in the pending list
	count := q.GetNackedItemsCount()
	assert.Equal(t, 1, count, "Should have 1 item in nacked list")

	// Acknowledge the item
	result := q.Acknowledge("ack-id")
	assert.True(t, result, "Acknowledge should succeed")

	// Verify item is removed from pending list
	count = q.GetNackedItemsCount()
	assert.Equal(t, 0, count, "Should have 0 items in nacked list after acknowledgment")

	// Test acknowledging non-existent item
	result = q.Acknowledge("non-existent")
	assert.True(t, result, "Acknowledge should succeed even for non-existent items")
}

func TestRequeueNackedItems(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// First add items to the main queue
	q.Enqueue("item-1")
	q.Enqueue("item-2")

	// Dequeue them and prepare for acknowledgment
	item1, ok := q.Dequeue()
	assert.True(t, ok, "First dequeue should succeed")
	item2, ok := q.Dequeue()
	assert.True(t, ok, "Second dequeue should succeed")

	// Prepare for acknowledgment
	err := q.PrepareForFutureAck("ack-1", item1)
	assert.NoError(t, err, "PrepareForFutureAck should succeed for item1")
	err = q.PrepareForFutureAck("ack-2", item2)
	assert.NoError(t, err, "PrepareForFutureAck should succeed for item2")

	// Verify main queue is empty
	assert.Equal(t, 0, q.Len(), "Main queue should be empty")

	// Verify items are in the nacked list
	count := q.GetNackedItemsCount()
	assert.Equal(t, 2, count, "Should have 2 items in nacked list")

	// Set a longer ack timeout (Redis minimum is 1s)
	q.SetAckTimeout(1 * time.Second)

	// Wait for timeout to expire (wait a bit longer than the timeout)
	time.Sleep(1500 * time.Millisecond)

	// Manually call RequeueNackedItems for testing
	type testQueue struct {
		*Queue
	}
	tq := testQueue{Queue: q}

	// Requeue the nacked items
	err = tq.RequeueNackedItems()
	assert.NoError(t, err, "RequeueNackedItems should succeed")

	// Verify items were moved from nacked list to main queue
	nackedCount := q.GetNackedItemsCount()
	assert.Equal(t, 0, nackedCount, "Should have 0 items in nacked list after requeue")

	mainQueueCount := q.Len()
	assert.Equal(t, 2, mainQueueCount, "Should have 2 items in main queue after requeue")

	// Verify we can dequeue the items again
	for i := range 2 {
		item, ok := q.Dequeue()
		assert.True(t, ok, fmt.Sprintf("Dequeue %d should succeed after requeue", i+1))

		bytes, ok := item.([]byte)
		assert.True(t, ok, fmt.Sprintf("Dequeued item %d should be []byte", i+1))

		// Convert to string for comparison
		strItem := string(bytes)
		assert.Contains(t, []string{"item-1", "item-2"}, strItem,
			"Dequeued item should be one of the requeued items")
	}

	// Verify main queue is empty again after dequeueing both items
	assert.Equal(t, 0, q.Len(), "Main queue should be empty after dequeueing requeued items")
}

func TestAcknowledgmentConcurrency(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Set a longer ack timeout for this test
	q.SetAckTimeout(5 * time.Second)

	// Number of concurrent workers and items
	const numWorkers = 5
	const itemsPerWorker = 20
	// Fill queue with items
	for i := range numWorkers * itemsPerWorker {
		assert.True(t, q.Enqueue(fmt.Sprintf("item-%d", i)), "Enqueue should succeed")
	}

	wg := sync.WaitGroup{}
	wg.Add(numWorkers)

	// Launch concurrent workers to process items with acknowledgment
	for w := range numWorkers {
		go func(workerID int) {
			defer wg.Done()

			for j := range itemsPerWorker {
				// Dequeue item
				item, ok := q.Dequeue()
				if !ok {
					break
				}

				// Create unique ack ID
				ackID := fmt.Sprintf("worker-%d-item-%d", workerID, j)

				// Prepare for acknowledgment
				err := q.PrepareForFutureAck(ackID, item)
				assert.NoError(t, err, "PrepareForFutureAck should succeed")

				// Acknowledge successful processing
				assert.True(t, q.Acknowledge(ackID), "Acknowledge should succeed")
			}
		}(w)
	}

	// Wait for all workers to finish
	wg.Wait()

	// Verify all items were processed
	assert.Equal(t, 0, q.Len(), "Main queue should be empty")
	assert.Equal(t, 0, q.GetNackedItemsCount(), "Nacked list should be empty")
}

func TestQueueRemove(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Test items
	testItems := []string{"item1", "item2", "item3"}

	// Enqueue items
	for _, item := range testItems {
		assert.True(t, q.Enqueue(item), "Enqueue should succeed")
	}

	// Verify initial queue length
	assert.Equal(t, len(testItems), q.Len(), "Queue length should match")

	// Get values to find the serialized version of item2
	values := q.Values()
	assert.Len(t, values, len(testItems), "Values should return all items")

	item2Bytes := []byte(testItems[1])

	require.NotNil(t, item2Bytes, "Failed to find item2 in queue values")

	// Remove item2 from the queue
	assert.True(t, q.Remove(item2Bytes), "Remove should succeed")

	// Verify queue length decreased by 1
	assert.Equal(t, len(testItems)-1, q.Len(), "Queue length should decrease after removal")

	// Verify item2 is not in the remaining items
	var foundItem2 bool
	for _, val := range q.Values() {
		if string(val.([]byte)) == "item2" {
			foundItem2 = true
			break
		}
	}
	assert.False(t, foundItem2, "item2 should have been removed from the queue")

	// Attempt to remove an item that doesn't exist
	assert.True(t, q.Remove([]byte("nonexistent-item")), "Remove should return true even for non-existent items")

	// Verify queue length hasn't changed
	assert.Equal(t, len(testItems)-1, q.Len(), "Queue length should not change when removing non-existent item")
}

func TestDequeueWithAckId(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Test with empty queue
	item, ok, ackID := q.DequeueWithAckId()
	assert.Nil(t, item, "Item should be nil when queue is empty")
	assert.False(t, ok, "ok should be false when queue is empty")
	assert.Equal(t, "", ackID, "ackID should be empty when queue is empty")

	// Add items to the queue
	testItem := "test-item-for-ack"
	assert.True(t, q.Enqueue(testItem), "Enqueue should succeed")

	// Dequeue with acknowledgment
	dequeuedItem, ok, ackID := q.DequeueWithAckId()
	assert.True(t, ok, "Dequeue should succeed")
	
	// Check if the item is a byte slice, which is the expected behavior 
	// when Redis deserializes the data
	if byteItem, isByteSlice := dequeuedItem.([]byte); isByteSlice {
		assert.Equal(t, testItem, string(byteItem), "Dequeued item should match enqueued item when converted to string")
	} else {
		assert.Equal(t, testItem, dequeuedItem, "Dequeued item should match enqueued item")
	}
	
	assert.NotEmpty(t, ackID, "Should generate a non-empty ackID")

	// Verify queue is now empty
	assert.Equal(t, 0, q.Len(), "Queue should be empty after dequeue")

	// Verify item is in the nacked items list
	count := q.GetNackedItemsCount()
	assert.Equal(t, 1, count, "Should have 1 item in nacked list")

	// Acknowledge the item
	result := q.Acknowledge(ackID)
	assert.True(t, result, "Acknowledge should succeed")

	// Verify item is removed from nacked list
	count = q.GetNackedItemsCount()
	assert.Equal(t, 0, count, "Should have 0 items in nacked list after acknowledgment")

	// Test error handling during acknowledgment preparation
	// Close the queue to force an error in PrepareForFutureAck
	q.Close()

	// Try to dequeue with acknowledgment when queue is closed
	// First, we need to add an item to the queue for testing
	// Create a new queue since the previous one is closed
	q2, cleanup2 := setupTestQueue(t)
	defer cleanup2()

	assert.True(t, q2.Enqueue(testItem), "Enqueue should succeed")

	// Now close the queue before attempting DequeueWithAckId
	q2.Close()

	// This should fail because the queue is closed
	_, ok, _ = q2.DequeueWithAckId()
	assert.False(t, ok, "DequeueWithAckId should fail when queue is closed")
}
