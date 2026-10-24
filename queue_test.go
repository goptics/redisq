package redisq

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testQueueKey = "test_queue"

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
	require.NoError(t, q.client.Del(ctx, q.getTimeoutKey()).Err(), "Failed to clear timeout key")

	cleanup := func() {
		q.Close() // first: it requeues what the test left pending
		q.client.Del(ctx, testQueueKey)
		q.client.Del(ctx, q.getNackedItemKey())
		q.client.Del(ctx, q.getTimeoutKey())
		q.client.Del(ctx, q.getHeartbeatKey())
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
	assert.Equal(t, 5*time.Minute, q.getVisibilityTimeout(), "Default visibility timeout should be 5 minutes")
}

func TestNewClientTimings(t *testing.T) {
	options := func(url string) *redis.Options {
		qs := New(url).(*queues)
		defer qs.Close()
		return qs.client.Options()
	}

	opts := options(getTestRedisURL())
	assert.Equal(t, 3*time.Second, opts.ReadTimeout)
	assert.Equal(t, 3*time.Second, opts.WriteTimeout)
	assert.Equal(t, 4*time.Second, opts.PoolTimeout)
	assert.Equal(t, 512*time.Millisecond, opts.MaxRetryBackoff)
	assert.Equal(t, 1, opts.DialerRetries)

	// Timings the URL sets win, including a disabled read timeout.
	opts = options(getTestRedisURL() + "?read_timeout=10s&max_retry_backoff=2s")
	assert.Equal(t, 10*time.Second, opts.ReadTimeout)
	assert.Equal(t, 10*time.Second, opts.WriteTimeout)
	assert.Equal(t, 2*time.Second, opts.MaxRetryBackoff)
	assert.Equal(t, time.Duration(0), options(getTestRedisURL()+"?read_timeout=0").ReadTimeout)
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

func TestPrepareForFutureAckWithTimeout(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Set both visibility timeout and ack timeout
	q.SetVisibilityTimeout(1 * time.Second)
	q.SetAckTimeout(5 * time.Second)

	err := q.PrepareForFutureAck("ack-with-timeout", "test-item")
	assert.NoError(t, err, "PrepareForFutureAck should succeed with timeouts set")

	// Verify item is in the nacked items list
	count := q.GetNackedItemsCount()
	assert.Equal(t, 1, count, "Should have 1 item in nacked list")

	// Verify item is also in the timeout ZSET
	ctx := context.Background()
	timeoutKey := q.queueKey + ":timeouts"
	zCount, err := q.client.ZCard(ctx, timeoutKey).Result()
	assert.NoError(t, err, "ZCard should succeed")
	assert.Equal(t, int64(1), zCount, "Should have 1 item in timeout ZSET")
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

	// Set short visibility timeout for testing
	q.SetVisibilityTimeout(100 * time.Millisecond)

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

func TestVisibilityTimeout(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Set visibility timeout
	visibilityTimeout := 2 * time.Second
	q.SetVisibilityTimeout(visibilityTimeout)

	// Enqueue item
	item := "test-visibility"
	assert.True(t, q.Enqueue(item), "Enqueue should succeed")

	// Dequeue with AckID
	dequeuedItem, ok, ackID := q.DequeueWithAckId()
	assert.True(t, ok, "Dequeue should succeed")
	if byteItem, isByteSlice := dequeuedItem.([]byte); isByteSlice {
		assert.Equal(t, item, string(byteItem))
	} else {
		assert.Equal(t, item, dequeuedItem)
	}

	// Verify item is in ZSET with correct score
	// Check ZSET
	timeoutKey := q.queueKey + ":timeouts"
	score, err := q.client.ZScore(context.Background(), timeoutKey, ackID).Result()
	assert.NoError(t, err, "ZScore should succeed")

	// Score should be roughly now + visibilityTimeout
	expectedScore := float64(time.Now().Add(visibilityTimeout).Unix())
	assert.InDelta(t, expectedScore, score, 2.0, "Score should be close to expected execution time")

	// Verify RequeueNackedItems does NOT requeue immediately
	err = q.RequeueNackedItems()
	assert.NoError(t, err)
	assert.Equal(t, 0, q.Len(), "Queue should be empty as timeout hasn't passed")

	// Wait for timeout
	time.Sleep(visibilityTimeout + 500*time.Millisecond)

	// Verify RequeueNackedItems DOES requeue now
	err = q.RequeueNackedItems()
	assert.NoError(t, err)
	assert.Equal(t, 1, q.Len(), "Queue should have 1 item after timeout")

	// Verify item can be dequeued again
	dequeuedItem2, ok2, _ := q.DequeueWithAckId()
	assert.True(t, ok2, "Should be able to dequeue again")
	if byteItem, isByteSlice := dequeuedItem2.([]byte); isByteSlice {
		assert.Equal(t, item, string(byteItem))
	} else {
		assert.Equal(t, item, dequeuedItem2)
	}
}

func TestQueuePurge(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Add items to the queue
	testData := []string{"item1", "item2", "item3"}
	for _, item := range testData {
		assert.True(t, q.Enqueue(item), "Enqueue should succeed")
	}

	// Verify items are in the queue
	assert.Equal(t, len(testData), q.Len(), "Queue should have items")

	// Purge the queue
	q.Purge()

	// Verify queue is empty
	assert.Equal(t, 0, q.Len(), "Queue should be empty after purge")

	// Verify dequeue returns nothing
	_, ok := q.Dequeue()
	assert.False(t, ok, "Dequeue should return false after purge")
}

func TestRequeueNackedItemsClosed(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Close the queue
	q.Close()

	// Attempt to requeue nacked items
	err := q.RequeueNackedItems()
	assert.Error(t, err, "RequeueNackedItems should fail when queue is closed")
	assert.Contains(t, err.Error(), "queue closed", "Error should mention queue closed")
}

func TestRequeueNackedItemsWithoutVisibilityTimeout(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Disable visibility timeout
	q.SetVisibilityTimeout(0)

	// Add items to the queue
	q.Enqueue("item1")
	q.Enqueue("item2")

	// Manually add items to nacked list (simulating failed acks)
	ctx := context.Background()
	q.client.HSet(ctx, q.getNackedItemKey(), "test-ack-1", "nacked-item-1")
	q.client.HSet(ctx, q.getNackedItemKey(), "test-ack-2", "nacked-item-2")

	// Verify nacked items are there
	assert.Equal(t, 2, q.GetNackedItemsCount(), "Should have 2 items in nacked list")

	// Requeue nacked items (without visibility timeout, should get all items)
	err := q.RequeueNackedItems()
	assert.NoError(t, err, "RequeueNackedItems should succeed")

	// Verify nacked list is now empty
	assert.Equal(t, 0, q.GetNackedItemsCount(), "Nacked list should be empty after requeue")

	// Verify items were requeued (queue should have original 2 + requeued 2 = 4)
	assert.Equal(t, 4, q.Len(), "Queue should have 4 items after requeue")
}

func TestRequeueNackedItemsEmptyWithVisibilityTimeout(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	// Set visibility timeout
	q.SetVisibilityTimeout(5 * time.Minute)

	// No items in nacked list, this should return nil early
	err := q.RequeueNackedItems()
	assert.NoError(t, err, "RequeueNackedItems should succeed with empty nacked list")

	// Verify queue is still empty
	assert.Equal(t, 0, q.Len(), "Queue should be empty")
}

// An item popped with an ack ID must never be in neither the queue nor the
// pending hash, or a client crash at that moment would lose it.
func TestDequeueWithAckIdNeverDropsItem(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	const n = 300
	for i := range n {
		require.True(t, q.Enqueue(fmt.Sprintf("item-%d", i)))
	}

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range n {
			_, ok, _ := q.DequeueWithAckId()
			assert.True(t, ok)
		}
	}()

	for {
		select {
		case <-done:
			return
		default:
		}
		var llen, hlen *redis.IntCmd
		_, err := q.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
			llen = p.LLen(ctx, q.queueKey)
			hlen = p.HLen(ctx, q.getNackedItemKey())
			return nil
		})
		require.NoError(t, err)
		if got := llen.Val() + hlen.Val(); got != n {
			t.Errorf("queue+pending = %d, want %d: an item was in neither", got, n)
			<-done
			return
		}
	}
}

// Unacknowledged jobs must outlive the ack timeout so they can be requeued;
// it used to expire the whole pending hash and delete them.
func TestAckTimeoutKeepsPendingForRequeue(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	q.SetAckTimeout(time.Second)
	require.True(t, q.Enqueue("a"))
	require.True(t, q.Enqueue("b"))
	for range 2 {
		_, ok, _ := q.DequeueWithAckId()
		require.True(t, ok)
	}

	time.Sleep(2100 * time.Millisecond)
	require.NoError(t, q.RequeueNackedItems()) // the sweeper may have done it already
	assert.Equal(t, 2, q.Len(), "expired jobs should be back in the queue")
	assert.Equal(t, 0, q.GetNackedItemsCount())
}

// SetAckTimeout(0) meant "no expiry"; it must not turn the visibility timeout
// off, which would requeue jobs that are still being processed.
func TestAckTimeoutZeroKeepsVisibility(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	q.SetVisibilityTimeout(time.Minute)
	q.SetAckTimeout(0)
	require.True(t, q.Enqueue("job"))
	_, ok, _ := q.DequeueWithAckId()
	require.True(t, ok)

	require.NoError(t, q.RequeueNackedItems())
	assert.Equal(t, 0, q.Len(), "an in-progress job was requeued")
	assert.Equal(t, 1, q.GetNackedItemsCount())
}

// Several processes requeueing the same expired items at once must push each
// item back exactly once.
func TestConcurrentRequeueNoDuplicates(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	const n = 200
	q.SetVisibilityTimeout(100 * time.Millisecond)
	for i := range n {
		require.True(t, q.Enqueue(fmt.Sprintf("item-%d", i)))
	}
	for range n {
		_, ok, _ := q.DequeueWithAckId()
		require.True(t, ok)
	}
	time.Sleep(1100 * time.Millisecond)

	var wg sync.WaitGroup
	for range 8 {
		qs := New(getTestRedisURL()).(*queues)
		defer qs.Close()
		other := newQueueBase(qs.client, testQueueKey) // NewQueue would requeue on construction
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, other.RequeueNackedItems())
		}()
	}
	wg.Wait()

	assert.Equal(t, n, q.Len(), "each expired item should be requeued once")
	assert.Equal(t, 0, q.GetNackedItemsCount())
}

// Closing the client without closing the queue must stop the sweeper, not
// log a failed requeue every second forever.
func TestSweeperStopsWhenClientClosed(t *testing.T) {
	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	qs := New(getTestRedisURL())
	_ = qs.NewQueue("test_sweeper_closed_client")
	require.NoError(t, qs.Close())
	time.Sleep(3500 * time.Millisecond)

	assert.LessOrEqual(t, strings.Count(buf.String(), "Error requeueing"), 1)
}

// lockedBuffer collects log output written by other goroutines.
type lockedBuffer struct {
	mx  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mx.Lock()
	defer b.mx.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mx.Lock()
	defer b.mx.Unlock()
	return b.buf.String()
}

// A job must stay pending for its whole visibility timeout. Deadlines rounded
// down to whole seconds let it be requeued up to a second early.
func TestVisibilityTimeoutNotCutShort(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	q.SetVisibilityTimeout(time.Second)
	// Start half way into a second, where a whole-second deadline would pass
	// after about 500ms.
	for ns := time.Now().Nanosecond(); ns < 5e8 || ns > 6e8; ns = time.Now().Nanosecond() {
		time.Sleep(5 * time.Millisecond)
	}
	require.True(t, q.Enqueue("job"))
	start := time.Now()
	_, ok, _ := q.DequeueWithAckId()
	require.True(t, ok)

	for q.GetNackedItemsCount() == 1 && time.Since(start) < 3*time.Second {
		require.NoError(t, q.RequeueNackedItems())
		time.Sleep(20 * time.Millisecond)
	}

	assert.GreaterOrEqual(t, time.Since(start), 990*time.Millisecond, "requeued before its visibility timeout")
	assert.Equal(t, 1, q.Len())
}

// pendingStep makes items pending one way: a single pop, a batch pop of one
// (the lean script), a batch pop, or PrepareForFutureAck.
type pendingStep struct {
	name string
	run  func() []string // the ack IDs it made pending
}

// pendingSteps returns the four steps for c's queue q, which must hold at
// least 4 items.
func pendingSteps(t *testing.T, c retryCase, q *Queue) []pendingStep {
	b := batcherFor(c, q)
	one := b.(interface{ DequeueWithAckId() (any, bool, string) })
	batch := func(slots int) []string {
		n, _, err := b.DequeueBatchWithAckIds(make([]any, slots), make([]string, slots))
		require.NoError(t, err)
		require.Equal(t, slots, n)
		resv := fmt.Sprintf("%020d", q.ackSeq.Load())
		return []string{q.ackIDOf(resv, 1), q.ackIDOf(resv, slots)}
	}
	return []pendingStep{
		{"single pop", func() []string { _, ok, id := one.DequeueWithAckId(); require.True(t, ok); return []string{id} }},
		{"batch pop of one", func() []string { return batch(1) }},
		{"batch pop", func() []string { return batch(2) }},
		{"prepare", func() []string {
			require.NoError(t, q.PrepareForFutureAck("prepared", "x"))
			return []string{"prepared"}
		}},
	}
}

// A pop or prepare that waits in Redis gets its visibility deadline from
// when it runs. One stamped when the client sent it started the timeout early
// by the wait, so the sweeper could requeue an item its consumer just got.
func TestDeadlineCountsFromWhenItRuns(t *testing.T) {
	const vt, pause = time.Second, 1500 * time.Millisecond
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			q, push, cleanup := c.setup(t, getTestRedisURL())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			q.SetVisibilityTimeout(vt)
			for _, v := range []string{"a", "b", "c", "d"} {
				require.True(t, push(v))
			}

			opts, err := redis.ParseURL(getTestRedisURL())
			require.NoError(t, err)
			admin := redis.NewClient(opts)
			defer admin.Close()

			for _, s := range pendingSteps(t, c, q) {
				require.NoError(t, admin.Do(ctx, "CLIENT", "PAUSE", pause.Milliseconds()).Err())
				sent := time.Now()
				ids := s.run()
				require.GreaterOrEqual(t, time.Since(sent), pause-200*time.Millisecond, "%s: Redis did not hold it", s.name)
				min := unixSeconds(time.Now().Add(vt - 100*time.Millisecond))
				max := unixSeconds(time.Now().Add(vt + 100*time.Millisecond))
				for _, id := range ids {
					d, err := q.client.ZScore(ctx, q.getTimeoutKey(), id).Result()
					require.NoError(t, err, s.name)
					assert.GreaterOrEqual(t, d, min, "%s: deadline counted from when it was sent", s.name)
					assert.LessOrEqual(t, d, max, "%s: deadline past the visibility timeout", s.name)
				}
			}
		})
	}
}

// With no visibility timeout, items go pending without a deadline, so no
// sweeper ever requeues them.
func TestNoVisibilityTimeoutStoresNoDeadline(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			q, push, cleanup := c.setup(t, getTestRedisURL())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			q.SetVisibilityTimeout(0)
			for _, v := range []string{"a", "b", "c", "d"} {
				require.True(t, push(v))
			}

			for _, s := range pendingSteps(t, c, q) {
				s.run()
			}
			assert.Equal(t, 5, q.GetNackedItemsCount())
			assert.Zero(t, q.client.ZCard(context.Background(), q.getTimeoutKey()).Val(), "a deadline was stored")
		})
	}
}

// Redis 6 configured to replicate scripts verbatim rejects writes after a
// non-deterministic command (TIME, SPOP) unless the script replicates its
// effects. Redis 7 dropped the setting, so this runs only on 6 (REDIS_TAG=6.0).
func TestScriptsUnderVerbatimReplication(t *testing.T) {
	ctx := context.Background()
	opts, err := redis.ParseURL(getTestRedisURL())
	require.NoError(t, err)
	admin := redis.NewClient(opts)
	defer admin.Close()
	if err := admin.ConfigSet(ctx, "lua-replicate-commands", "no").Err(); err != nil {
		t.Skipf("no lua-replicate-commands setting: %v", err)
	}
	defer admin.ConfigSet(ctx, "lua-replicate-commands", "yes")

	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			q, push, cleanup := c.setup(t, getTestRedisURL())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			for _, v := range []string{"a", "b", "c", "d"} {
				require.True(t, push(v))
			}

			q.SetVisibilityTimeout(300 * time.Millisecond)
			for _, s := range pendingSteps(t, c, q) {
				s.run()
			}
			assert.EqualValues(t, 5, q.client.ZCard(ctx, q.getTimeoutKey()).Val(), "every pending item has its deadline")

			time.Sleep(400 * time.Millisecond)
			if c.name == "priority" {
				require.NoError(t, (&PriorityQueue{Queue: q}).RequeueNackedItems())
			} else {
				require.NoError(t, q.RequeueNackedItems())
			}
			assert.Zero(t, q.GetNackedItemsCount(), "expired items are requeued")

			// The sweep, past a dead and a gone owner (counting subscribers,
			// which Redis 6 flags as non-deterministic), and the release Close
			// runs. The clock is a second old, so this sweep counts.
			hb, pres := q.getHeartbeatKey(), q.getPresenceKey()
			now := float64(q.client.Time(ctx).Val().UnixMicro()) / 1e6
			require.NoError(t, q.client.Set(ctx, hb+":clock", now-1, 0).Err())
			require.NoError(t, q.client.ZAdd(ctx, hb, redis.Z{Score: 1, Member: "dead"}, redis.Z{Score: now + 60, Member: "gone"}).Err())
			require.NoError(t, q.client.ZAdd(ctx, pres, redis.Z{Score: now - 60, Member: "gone"}).Err())
			require.NoError(t, q.client.HSet(ctx, q.getNackedItemKey(), "dead:1:0001", "x", "gone:1:0001", "z", q.ackPrefix+":1:0001", "y").Err())
			keys := append(q.requeueKeys(), hb, hb+":clock", pres)
			require.NoError(t, q.tickScript.Run(ctx, q.client, keys, q.ackPrefix, q.hbt(), sweepGap.Seconds(), q.queueKey+":wake:", presenceGrace.Seconds()).Err())
			require.NoError(t, q.release())
			// The queue's own sweep may have moved the clock on in between;
			// its next one counts then.
			assert.Eventually(t, func() bool { return q.GetNackedItemsCount() == 0 }, 3*time.Second, 50*time.Millisecond,
				"the dead and gone owners' and the released items are requeued")
		})
	}

	t.Run("wake", func(t *testing.T) {
		qs := New(getTestRedisURL())
		defer qs.Close()
		q := qs.NewDistributedQueue("test_verbatim_wake")
		defer q.Close()
		delKeys(t, q.Queue.client, "test_verbatim_wake")
		defer delKeys(t, q.Queue.client, "test_verbatim_wake")
		require.NoError(t, q.Queue.client.SAdd(ctx, q.idleKey, "gone-1", "gone-2").Err())
		assert.True(t, q.Enqueue("a"), "waking past consumers that are gone")
		require.NoError(t, q.Queue.client.SAdd(ctx, q.idleKey, "gone-1", "gone-2").Err())
		assert.True(t, q.EnqueueOnce("b", "x"), "the same with an id")

		pq := qs.NewDistributedPriorityQueue("test_verbatim_wake_prio")
		defer pq.Close()
		delKeys(t, pq.Queue.client, "test_verbatim_wake_prio")
		defer delKeys(t, pq.Queue.client, "test_verbatim_wake_prio")
		require.NoError(t, pq.Queue.client.SAdd(ctx, pq.idleKey, "gone-1", "gone-2").Err())
		assert.True(t, pq.Enqueue("a", 1), "priority: waking past consumers that are gone")
		require.NoError(t, pq.Queue.client.SAdd(ctx, pq.idleKey, "gone-1", "gone-2").Err())
		assert.True(t, pq.EnqueueOnce("b", 1, "x"), "priority: the same with an id")
	})
}

// dropProxy forwards TCP to Redis. It watches for one script, by hash: each
// call of it that reaches Redis counts in sends, and while drops > 0 the
// call runs on Redis but its connection is cut before the reply gets back,
// the way a network blip or a read timeout does. go-redis then re-sends it.
type dropProxy struct {
	ln     net.Listener
	target string
	hashes [][]byte
	sends  atomic.Int32
	drops  atomic.Int32
	onDrop func() // if set, runs after Redis ran a call whose reply is being dropped
}

// newDropProxy drops the replies of calls to any of scripts.
func newDropProxy(t *testing.T, scripts ...*redis.Script) *dropProxy {
	t.Helper()
	opts, err := redis.ParseURL(getTestRedisURL())
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	p := &dropProxy{ln: ln, target: opts.Addr}
	for _, sc := range scripts {
		p.hashes = append(p.hashes, []byte(sc.Hash()))
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(c)
		}
	}()

	return p
}

func (p *dropProxy) url() string { return "redis://" + p.ln.Addr().String() }

func (p *dropProxy) handle(c net.Conn) {
	r, err := net.Dial("tcp", p.target)
	if err != nil {
		c.Close()
		return
	}
	defer c.Close()
	defer r.Close()

	var drop atomic.Bool
	go func() { // client -> Redis
		buf := make([]byte, 64<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if slices.ContainsFunc(p.hashes, func(h []byte) bool { return bytes.Contains(buf[:n], h) }) {
					p.sends.Add(1)
					if p.drops.Add(-1) >= 0 {
						drop.Store(true)
					}
				}
				if _, err := r.Write(buf[:n]); err != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		r.Close()
	}()

	buf := make([]byte, 64<<10)
	for { // Redis -> client
		n, err := r.Read(buf)
		if n > 0 {
			if drop.Load() {
				if p.onDrop != nil {
					p.onDrop()
				}
				return // Redis ran the script; the client never hears back
			}
			if _, err := c.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// retryCase is a list or priority queue behind a dropProxy, holding "a"
// then "b" at the same priority.
type retryCase struct {
	name      string
	pop       *redis.Script // batch pop
	popOne    *redis.Script // single pop
	popOneLen *redis.Script // batch pop of one item
	setup     func(t *testing.T, url string) (q *Queue, push func(string) bool, cleanup func())
}

var retryCases = []retryCase{
	{"list", listAckPop, listAckPopOne, listAckPopOneLen, func(t *testing.T, url string) (*Queue, func(string) bool, func()) {
		qs := New(url)
		q := qs.NewQueue("test_retry_pop_list")
		return q, func(s string) bool { return q.Enqueue(s) }, func() { q.Close(); qs.Close() }
	}},
	{"priority", zsetAckPop, zsetAckPopOne, zsetAckPopOneLen, func(t *testing.T, url string) (*Queue, func(string) bool, func()) {
		qs := New(url)
		pq := qs.NewPriorityQueue("test_retry_pop_prio")
		return pq.Queue, func(s string) bool { return pq.Enqueue(s, 1) }, func() { pq.Close(); qs.Close() }
	}},
}

func (c retryCase) run(t *testing.T, drops int32, check func(t *testing.T, q *Queue, p *dropProxy, dequeue func() (string, bool, string), size func() int)) {
	t.Run(c.name, func(t *testing.T) {
		ctx := context.Background()
		p := newDropProxy(t, c.popOne)
		q, push, cleanup := c.setup(t, p.url())
		defer cleanup()

		keys := []string{q.queueKey, q.queueKey + ":seq", q.getNackedItemKey(), q.getTimeoutKey(), q.getPriorityKey()}
		q.client.Del(ctx, keys...)
		defer q.client.Del(ctx, keys...)

		require.True(t, push("a"))
		require.True(t, push("b"))
		require.NoError(t, c.popOne.Load(ctx, q.client).Err()) // so every pop is an EVALSHA the proxy sees

		dequeue := func() (string, bool, string) {
			var item any
			var ok bool
			var ackID string
			if c.name == "priority" {
				item, ok, ackID = (&PriorityQueue{Queue: q}).DequeueWithAckId()
			} else {
				item, ok, ackID = q.DequeueWithAckId()
			}
			if !ok {
				return "", false, ""
			}
			return string(item.([]byte)), true, ackID
		}

		size := q.Len
		if c.name == "priority" {
			size = (&PriorityQueue{Queue: q}).Len
		}

		p.drops.Store(drops)
		check(t, q, p, dequeue, size)
	})
}

// A pop whose reply is lost is re-sent by go-redis. The re-run must hand back
// the item the first run popped, not pop the next one under the same ack ID,
// which used to lose the first item.
func TestPopRetryAfterLostReplyKeepsEveryItem(t *testing.T) {
	for _, c := range retryCases {
		c.run(t, 1, func(t *testing.T, q *Queue, p *dropProxy, dequeue func() (string, bool, string), size func() int) {
			item, ok, ackID := dequeue()
			require.True(t, ok)
			assert.EqualValues(t, 2, p.sends.Load(), "the pop should run, lose its reply and be re-sent once")

			assert.Equal(t, "a", item)
			assert.Equal(t, 1, size(), "b must still be queued")
			assert.Equal(t, 1, q.GetNackedItemsCount())
			pending, err := q.client.HGet(context.Background(), q.getNackedItemKey(), ackID).Result()
			require.NoError(t, err)
			assert.Equal(t, "a", pending)
			if c.name == "priority" {
				score, err := q.client.HGet(context.Background(), q.getPriorityKey(), ackID).Result()
				require.NoError(t, err)
				assert.Equal(t, "1", score, "the priority must be kept for a requeue")
			}
		})
	}
}

// When every attempt loses its reply, the call fails but its pop ran. The
// next call reuses the ack ID and gets that item back at once, instead of it
// waiting pending for the visibility timeout.
func TestPopAfterFailedCallReturnsItsItem(t *testing.T) {
	for _, c := range retryCases {
		c.run(t, 4, func(t *testing.T, q *Queue, p *dropProxy, dequeue func() (string, bool, string), size func() int) { // 4 = go-redis's 1 try + 3 retries
			_, ok, _ := dequeue()
			require.False(t, ok, "every reply was dropped")
			assert.EqualValues(t, 4, p.sends.Load())
			assert.Equal(t, 1, q.GetNackedItemsCount(), "the first attempt popped a")

			item, ok, _ := dequeue()
			require.True(t, ok)
			assert.Equal(t, "a", item)
			assert.Equal(t, 1, size(), "b must still be queued")
			assert.Equal(t, 1, q.GetNackedItemsCount())

			item, ok, _ = dequeue()
			require.True(t, ok)
			assert.Equal(t, "b", item, "the next call is an ordinary pop")
		})
	}
}

// slowPipelines delays every pipeline by d, like a distant Redis.
type slowPipelines struct{ d time.Duration }

func (s slowPipelines) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (s slowPipelines) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (s slowPipelines) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		time.Sleep(s.d)
		return next(ctx, cmds)
	}
}

// Workers acknowledge in parallel; a queue lock held across the network call
// used to make each ack wait for the one before it.
func TestAcknowledgeRunsInParallel(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()

	const n = 10
	ackIDs := make([]string, 0, n)
	for i := range n {
		require.True(t, q.Enqueue(fmt.Sprint(i)))
		_, ok, ackID := q.DequeueWithAckId()
		require.True(t, ok)
		ackIDs = append(ackIDs, ackID)
	}

	q.client.AddHook(slowPipelines{100 * time.Millisecond})

	start := time.Now()
	var wg sync.WaitGroup
	for _, id := range ackIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.True(t, q.Acknowledge(id))
		}()
	}
	wg.Wait()

	assert.Less(t, time.Since(start), 500*time.Millisecond, "acks ran one at a time")
	assert.Equal(t, 0, q.GetNackedItemsCount())
}

// SetHeartbeatTimeout keeps a floor of a few sweeps, and 0 or less turns the
// heartbeat off. A queue whose heartbeat goes missing (another process took
// it for dead) notices on its next sweep, and a sweep that fails keeps
// running.
func TestHeartbeatSetterAndLapse(t *testing.T) {
	ctx := context.Background()
	qs := New(getTestRedisURL())
	defer qs.Close()
	q := qs.NewQueue("test_heartbeat_lapse")
	defer q.Close()
	delKeys(t, q.client, q.queueKey)
	defer delKeys(t, q.client, q.queueKey)

	q.SetHeartbeatTimeout(time.Second)
	assert.Equal(t, minHeartbeat.Seconds(), q.hbt(), "raised to the floor")
	q.SetHeartbeatTimeout(-time.Second)
	assert.Zero(t, q.hbt(), "off")
	q.SetHeartbeatTimeout(time.Minute)
	assert.Equal(t, 60.0, q.hbt())

	require.True(t, q.Enqueue("job"))
	_, ok, ackID := q.DequeueWithAckId()
	require.True(t, ok)
	require.True(t, q.beating.Load())
	require.NoError(t, q.client.Del(ctx, q.getHeartbeatKey()).Err())
	assert.Eventually(t, func() bool { return !q.beating.Load() }, 3*time.Second, 20*time.Millisecond, "the lapse went unnoticed")

	// A key of the wrong type fails every sweep until it is fixed.
	require.NoError(t, q.client.Set(ctx, q.getHeartbeatKey(), "not a zset", 0).Err())
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, q.client.Del(ctx, q.getHeartbeatKey()).Err())
	require.NoError(t, q.client.ZAdd(ctx, q.getTimeoutKey(), redis.Z{Score: 1, Member: ackID}).Err())
	assert.Eventually(t, func() bool { return q.Len() == 1 }, 3*time.Second, 20*time.Millisecond, "the sweep stopped after a failure")
}
