package redisq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goptics/varmq/contract"
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
	require.NoError(t, q.client.Del(ctx, q.queueKey).Err(), "Failed to clear test queue")
	require.NoError(t, q.client.Del(ctx, q.getNackedItemKey()).Err(), "Failed to clear nacked queue")
	require.NoError(t, q.client.Del(ctx, q.getTimeoutKey()).Err(), "Failed to clear timeout key")

	cleanup := func() {
		q.Close() // first: it requeues what the test left pending
		q.client.Del(ctx, q.queueKey)
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

	assert.Equal(t, "redisq:fifo:{"+testQueueKey+"}", q.queueKey, "Queue key should match")
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
	q.SetVisibilityTimeout(5 * time.Second)

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
	q.SetVisibilityTimeout(1 * time.Second)

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
	q.SetVisibilityTimeout(5 * time.Second)

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

	q.SetVisibilityTimeout(time.Second)
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
	q := qs.NewQueue("test_sweeper_closed_client") // producer only: its nudge loop never runs a script
	require.NoError(t, qs.Close())
	time.Sleep(3500 * time.Millisecond)

	assert.LessOrEqual(t, strings.Count(buf.String(), "Error requeueing"), 1)
	assert.Error(t, q.Notification.ctx.Err(), "nudge loop and its ticker must stop too")
	assert.Error(t, q.ctx.Err(), "pause loop waits on q.ctx")

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		assert.NoError(t, q.Close())
		assert.NoError(t, q.Close())
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hangs after the client closed")
	}
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
			require.NoError(t, q.tickScript.Run(ctx, q.client, q.tickKeys(), q.ackPrefix, q.hbt(), sweepGap.Seconds(), q.queueKey+":wake:", presenceGrace.Seconds()).Err())
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
		q := qs.NewQueue("test_verbatim_wake")
		defer q.Close()
		delKeys(t, q.client, "test_verbatim_wake")
		defer delKeys(t, q.client, "test_verbatim_wake")
		require.NoError(t, q.client.SAdd(ctx, q.idleKey, "gone-1", "gone-2").Err())
		assert.True(t, q.Enqueue("a"), "waking past consumers that are gone")
		require.NoError(t, q.client.SAdd(ctx, q.idleKey, "gone-1", "gone-2").Err())
		assert.True(t, q.EnqueueOnce("b", "x"), "the same with an id")

		pq := qs.NewPriorityQueue("test_verbatim_wake_prio")
		defer pq.Close()
		delKeys(t, pq.client, "test_verbatim_wake_prio")
		defer delKeys(t, pq.client, "test_verbatim_wake_prio")
		require.NoError(t, pq.client.SAdd(ctx, pq.idleKey, "gone-1", "gone-2").Err())
		assert.True(t, pq.Enqueue("a", 1), "priority: waking past consumers that are gone")
		require.NoError(t, pq.client.SAdd(ctx, pq.idleKey, "gone-1", "gone-2").Err())
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

// plainCases open a plain queue of each storage on key, with its push
// (priority 3) and ack pop.
var plainCases = []struct {
	name string
	open func(qs Queues, key string) (*Queue, func(string) bool, func() (any, bool, string))
}{
	{"list", func(qs Queues, key string) (*Queue, func(string) bool, func() (any, bool, string)) {
		q := qs.NewQueue(key)
		return q, func(v string) bool { return q.Enqueue(v) }, q.DequeueWithAckId
	}},
	{"priority", func(qs Queues, key string) (*Queue, func(string) bool, func() (any, bool, string)) {
		q := qs.NewPriorityQueue(key)
		return q.Queue, func(v string) bool { return q.Enqueue(v, 3) }, q.DequeueWithAckId
	}},
}

// popWithin pops from a queue until it hands out an item, and fails after d.
func popWithin(t *testing.T, d time.Duration, pop func() (any, bool, string), msg string) (string, string) {
	t.Helper()
	start := time.Now()
	for {
		item, ok, ackID := pop()
		if ok {
			return string(item.([]byte)), ackID
		}
		require.Less(t, time.Since(start), d, msg)
		time.Sleep(50 * time.Millisecond)
	}
}

// A plain queue whose process dies without Close (its client closes, taking
// its pub/sub connection and sweep, as kill -9 would) gets the job it was
// running back once the process restarts, within the presence grace of the
// restart rather than the heartbeat timeout: Redis dropped its subscription,
// and the restarted process's first sweep sees that at once. The restart
// outlasts sweepGap, which moves heartbeats on but not presence.
func TestRestartedQueueGetsCrashedJobBack(t *testing.T) {
	for _, c := range plainCases {
		t.Run(c.name, func(t *testing.T) {
			key := "test_restart_" + c.name
			qs1 := New(getTestRedisURL())
			q1, push, pop := c.open(qs1, key)
			delKeys(t, q1.client, key)
			require.True(t, push("job"))
			_, ok, _ := pop()
			require.True(t, ok)
			seen(t, q1, q1.ackPrefix) // by its own sweep
			require.NoError(t, qs1.Close())
			time.Sleep(sweepGap + 500*time.Millisecond)

			qs2 := New(getTestRedisURL())
			defer qs2.Close()
			restarted := time.Now()
			q2, _, pop2 := c.open(qs2, key)
			defer q2.Close()
			defer delKeys(t, q2.client, key)
			item, ackID := popWithin(t, presenceGrace+time.Second, pop2, "the crashed job is not back")
			assert.Equal(t, "job", item)
			assert.True(t, q2.Acknowledge(ackID))
			t.Logf("back %v after the restart", time.Since(restarted).Round(10*time.Millisecond))
		})
	}
}

// Two live processes share a key, as in a rolling deploy, each running a
// job. One dies and restarts: its job comes back, and the other's, still
// running, is never requeued, as a blind requeue on start would do.
func TestRestartKeepsLiveProcessJobs(t *testing.T) {
	for _, c := range plainCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			key := "test_restart_live_" + c.name
			qsA, qsB := New(getTestRedisURL()), New(getTestRedisURL())
			defer qsB.Close()
			a, push, popA := c.open(qsA, key)
			delKeys(t, a.client, key)
			b, _, popB := c.open(qsB, key)
			defer b.Close()
			defer delKeys(t, b.client, key)
			require.True(t, push("a"))
			require.True(t, push("b"))
			_, ok, _ := popA()
			require.True(t, ok)
			itemB, ok, ackB := popB()
			require.True(t, ok)
			require.Equal(t, "b", string(itemB.([]byte)))
			seen(t, b, a.ackPrefix)
			seen(t, b, b.ackPrefix)

			require.NoError(t, qsA.Close())
			died := time.Now()
			qsA2 := New(getTestRedisURL())
			defer qsA2.Close()
			a2, _, popA2 := c.open(qsA2, key)
			defer a2.Close()
			item, ackID := popWithin(t, presenceGrace+3*time.Second, popA2, "the dead process's job is not back")
			assert.Equal(t, "a", item)
			assert.True(t, a2.Acknowledge(ackID))
			t.Logf("back %v after the kill", time.Since(died).Round(10*time.Millisecond))

			time.Sleep(2 * presenceGrace)
			_, ok, _ = popA2()
			assert.False(t, ok, "the live process's job was requeued")
			assert.True(t, b.client.HExists(ctx, b.getNackedItemKey(), ackB).Val(), "the live process's job left pending")
			assert.True(t, b.Acknowledge(ackB))
		})
	}
}

// A new queue object requeues the items past their deadline at once, into
// its own storage: a priority queue's go back into its sorted set.
func TestNewQueueRequeuesExpiredItems(t *testing.T) {
	for _, c := range plainCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			key := "test_new_requeues_" + c.name
			qs := New(getTestRedisURL())
			defer qs.Close()
			q1, push, pop := c.open(qs, key)
			defer q1.Close()
			delKeys(t, q1.client, key)
			defer delKeys(t, q1.client, key)
			q1.SetVisibilityTimeout(100 * time.Millisecond)
			q1.SetHeartbeatTimeout(0) // so only the deadline can bring it back
			require.True(t, push("job"))
			kind := q1.client.Type(ctx, q1.queueKey).Val()
			_, ok, _ := pop()
			require.True(t, ok)
			q1.cancel() // its sweep stops
			time.Sleep(200 * time.Millisecond)

			q2, _, pop2 := c.open(qs, key)
			defer q2.Close()
			assert.Equal(t, kind, q2.client.Type(ctx, q2.queueKey).Val(), "the key keeps its type")
			item, ok, _ := pop2()
			require.True(t, ok)
			assert.Equal(t, "job", string(item.([]byte)), "back before the sweep's first tick")
		})
	}
}

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
	assert.False(t, marked(t, q, id), "mark gone")
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
// record and outcome cancelled, and the pop goes on with the next item,
// under that item's own priority.
func TestCancelWaitingJob(t *testing.T) {
	pops := map[string]func(t *testing.T, q *Queue) (ids []string, ackID string){ // the ids handed out
		"raw": func(t *testing.T, q *Queue) ([]string, string) {
			v, ok := rawPopOf(q)
			require.True(t, ok)
			return []string{envID(t, v)}, ""
		},
		"single": func(t *testing.T, q *Queue) ([]string, string) {
			v, ok, ackID := ackPopOf(q)
			require.True(t, ok)
			return []string{envID(t, v)}, ackID
		},
		"batch one": func(t *testing.T, q *Queue) ([]string, string) { // the lean batch script
			items, ids := make([]any, 1), make([]string, 1)
			n, _, err := batchPopOf(q, items, ids)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			return []string{envID(t, items[0])}, ids[0]
		},
	}
	for name, pop := range pops {
		t.Run(name, func(t *testing.T) {
			eachCancelCase(t, func(t *testing.T, c cancelQueues) {
				got, _ := watch(c.prod, "a")
				require.True(t, c.push(envelope("a", `1`), 1))
				require.True(t, c.push(envelope("b", `1`), 5))
				require.NoError(t, c.prod.CancelJob("a"))

				ids, ackID := pop(t, c.cons)
				assert.Equal(t, []string{"b"}, ids)
				assertDropped(t, c.cons, "a")
				assert.Equal(t, contract.JobOutcome{ID: "a", State: contract.JobCancelled, Error: "cancelled"}, outcomeWithin(t, got, time.Second))
				assert.Zero(t, c.cons.Len())
				assert.LessOrEqual(t, c.cons.GetNackedItemsCount(), 1, "only b")
				if c.cons.isPriority {
					assert.LessOrEqual(t, c.cons.client.HLen(t.Context(), c.cons.getPriorityKey()).Val(), int64(1))
					if ackID != "" {
						assert.Equal(t, "5", c.cons.client.HGet(t.Context(), c.cons.getPriorityKey(), ackID).Val(), "b's priority, not a's")
					}
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
		assert.Zero(t, c.cons.client.Exists(t.Context(), c.cons.getCancelledKey()).Val(), "last ZREM drops the key")
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

// luaDecodable matches Redis's Lua cjson on JSON Go reads; text that isn't
// JSON passes.
func TestLuaDecodable(t *testing.T) {
	deep := func(n int) string { // nested n deep, the envelope included
		return `{"id":"d","data":` + strings.Repeat("[", n-1) + strings.Repeat("]", n-1) + `}`
	}
	cases := []struct {
		name, in string
		want     bool
	}{
		{"plain", envelope("a", `1`), true},
		{"pair", `{"id":"a","data":"\ud83d\ude00"}`, true},
		{"upper pair", `{"data":"\uD83D\uDE00"}`, true},
		{"escaped backslash", `{"data":"\\ud800"}`, true},
		{"bmp", `{"data":"\u00e9\uffff\"\n"}`, true},
		{"depth 1000", deep(1000), true},
		{"brackets in a string", `{"data":"` + strings.Repeat("[", 1001) + `"}`, true},
		{"lone high", `{"id":"a","data":"\ud800"}`, false},
		{"lone high, top level", `"\udbff"`, false},
		{"high, then text", `{"data":"\ud800x"}`, false},
		{"high, then bmp", `{"data":"\ud800\u0041"}`, false},
		{"high, high", `["\ud800\ud800"]`, false},
		{"lone low", `{"data":"\udc00"}`, false},
		{"upper lone low", `{"data":"\uDFFF"}`, false},
		{"depth 1001", deep(1001), false},
		{"text", "raw bytes", true},
		{"text, deep", strings.Repeat("[", 1001), true},
		{"text, lone escape", `say "\ud800`, true},
		{"wide, shallow", `[` + strings.Repeat(`{"a":[1]},`, 2000) + `{}]`, true},
		{"min pair", `"\ud800\udc00"`, true},
		{"max pair", `"\udbff\udfff"`, true},
		{"escaped quote, brackets in a string", `{"data":"\"` + strings.Repeat("[", 1001) + `"}`, true},
		{"text, bad hex", `"\uzz00"`, true},
		{"text, cut escape", `"\u12`, true},
	}
	q, cleanup := setupTestQueue(t)
	defer cleanup()
	decode := redis.NewScript(`return pcall(cjson.decode, ARGV[1]) and 1 or 0`)
	for _, c := range cases {
		assert.Equal(t, c.want, luaDecodable([]byte(c.in)), c.name)
		if json.Valid([]byte(c.in)) {
			n, err := decode.Run(t.Context(), q.client, nil, c.in).Int()
			require.NoError(t, err)
			assert.Equal(t, c.want, n == 1, "%s: cjson decodes it", c.name)
		}
	}
}

// Enqueue and EnqueueOnce refuse JSON Redis Lua can't decode; at the depth
// limit the pop's cancel gate still reads the id.
func TestEnqueueRefusesLuaUndecodable(t *testing.T) {
	nest := func(id string, n int) string {
		return `{"id":"` + id + `","data":` + strings.Repeat("[", n-1) + strings.Repeat("]", n-1) + `}`
	}
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		for _, v := range []string{`{"id":"s","data":"\ud800"}`, nest("s", 1001)} {
			assert.False(t, c.push(v, 1))
			assert.False(t, c.pushOnce(v, "s"))
		}
		assert.Zero(t, c.prod.client.Exists(ctx, c.prod.queueKey, c.prod.queueKey+":dedup:s").Val(), "nothing stored")

		require.True(t, c.push(nest("k", 1000), 1))
		require.NoError(t, c.prod.CancelJob("k"))
		_, ok, _ := ackPopOf(c.cons)
		assert.False(t, ok, "the gate read its id")
		assert.Equal(t, "cancelled", record(t, c.cons, "k")["state"])
		assert.True(t, c.push(`{"id":"w","data":[`+strings.Repeat(`{"a":[1]},`, 2000)+`{}]}`, 1), "wide, shallow")
	})
}

// A pop that drops the last waiting item finds the queue empty. With no
// record kept, the sweep can't catch up: the outcome comes from the pop's
// PUBLISH alone, in every pop.
func TestCancelOnlyItem(t *testing.T) {
	pops := map[string]func(q *Queue) bool{ // reports whether an item came out
		"raw":    func(q *Queue) bool { _, ok := rawPopOf(q); return ok },
		"single": func(q *Queue) bool { _, ok, _ := ackPopOf(q); return ok },
		"batch one": func(q *Queue) bool {
			n, _, err := batchPopOf(q, make([]any, 1), make([]string, 1))
			return err == nil && n > 0
		},
		"batch": func(q *Queue) bool { // want 2: the full batch script
			q.popLen.Store(2)
			n, _, err := batchPopOf(q, make([]any, 2), make([]string, 2))
			return err == nil && n > 0
		},
	}
	for name, pop := range pops {
		t.Run(name, func(t *testing.T) {
			eachCancelCase(t, func(t *testing.T, c cancelQueues) {
				c.cons.SetFailedRetention(0, 0)
				got, _ := watch(c.prod, "a")
				require.True(t, c.push(envelope("a", `1`), 1))
				require.NoError(t, c.prod.CancelJob("a"))
				assert.False(t, pop(c.cons))
				assert.Zero(t, c.cons.client.HLen(t.Context(), c.cons.getJobsKey()).Val(), "no retention: no record")
				assert.Equal(t, contract.JobOutcome{ID: "a", State: contract.JobCancelled, Error: "cancelled"}, outcomeWithin(t, got, time.Second))
			})
		})
	}
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
		assert.False(t, marked(t, c.cons, "gone"), "an ended job is not marked")

		require.NoError(t, c.prod.CancelJob("nobody"))
		assert.True(t, marked(t, c.cons, "nobody"))
		exp := c.cons.client.ZScore(ctx, c.cons.getCancelledKey(), "nobody").Val()
		now := float64(c.cons.client.Time(ctx).Val().UnixMilli())
		assert.InDelta(t, now+float64(cancelTTL.Milliseconds()), exp, 5000, "score: expiry in ms")
		ttl := c.cons.client.PTTL(ctx, c.cons.getCancelledKey()).Val()
		assert.Greater(t, ttl, 23*time.Hour, "backstop TTL")

		assert.Error(t, c.prod.CancelJob(""))
	})
}

// A running job's cancel reaches OnCancel in every process; the
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
		assert.ElementsMatch(t, []string{"r", "x1", "x2"}, calls())

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
		assert.False(t, marked(t, c.cons, "r"))
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

// A cancel message reaches OnCancel only while its job's mark is live: a
// stale one (mark gone or expired) is dropped.
func TestCancelStaleMessageDropped(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		var mu sync.Mutex
		var seen []string
		stop := c.cons.OnCancel(func(id string) {
			mu.Lock()
			seen = append(seen, id)
			mu.Unlock()
		})
		defer stop()
		calls := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
		publish := func(id string) {
			require.NoError(t, c.prod.client.Publish(ctx, c.prod.getControlKey(), cancelPrefix+id).Err())
		}
		now := c.cons.client.Time(ctx).Val().UnixMilli()
		mark := func(id string, exp int64) {
			require.NoError(t, c.cons.client.ZAdd(ctx, c.cons.getCancelledKey(), redis.Z{Score: float64(exp), Member: id}).Err())
		}

		mark("x", now+time.Hour.Milliseconds())
		publish("x") // first, so the subscription is known live
		require.Eventually(t, func() bool { return len(calls()) == 1 }, time.Second, 5*time.Millisecond)

		mark("old", now-1)
		publish("none")
		publish("old")
		mark("y", now+time.Hour.Milliseconds())
		publish("y")
		require.Eventually(t, func() bool { return len(calls()) == 2 }, time.Second, 5*time.Millisecond)
		assert.Never(t, func() bool { return len(calls()) > 2 }, 200*time.Millisecond, 10*time.Millisecond, "stale: no call")
		assert.Equal(t, []string{"x", "y"}, calls())
	})
}

// A stop that returns while the live check is under way calls nothing.
func TestCancelStopDuringLiveCheck(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		require.NoError(t, cancelLiveScript.Load(t.Context(), c.cons.client).Err()) // so the check is an EVALSHA
		h := &holdTick{hash: cancelLiveScript.Hash(), ran: make(chan struct{}), release: make(chan struct{})}
		c.cons.client.AddHook(h)
		var calls atomic.Int32
		stop := c.cons.OnCancel(func(string) { calls.Add(1) })
		h.armed.Store(true)

		require.NoError(t, c.prod.CancelJob("j"))
		select {
		case <-h.ran: // the check replied live; the reply waits
		case <-time.After(time.Second):
			t.Fatal("no live check")
		}
		stop()
		close(h.release)
		assert.Never(t, func() bool { return calls.Load() > 0 }, 300*time.Millisecond, 10*time.Millisecond, "fn after stop")
	})
}

// A live check that fails calls OnCancel anyway.
func TestCancelCheckErrorCallsHook(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		var buf lockedBuffer
		log.SetOutput(&buf)
		defer log.SetOutput(os.Stderr)
		var mu sync.Mutex
		var seen []string
		stop := c.cons.OnCancel(func(id string) { mu.Lock(); seen = append(seen, id); mu.Unlock() })
		defer stop()

		require.NoError(t, c.prod.client.Set(ctx, c.prod.getCancelledKey(), "x", 0).Err()) // ZSCORE: WRONGTYPE
		t.Cleanup(func() { c.prod.client.Del(context.Background(), c.prod.getCancelledKey()) })
		// publish again until the subscription is live
		var pubErr error
		require.Eventually(t, func() bool {
			if pubErr = c.prod.client.Publish(ctx, c.prod.getControlKey(), cancelPrefix+"e").Err(); pubErr != nil {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			return len(seen) > 0
		}, time.Second, 50*time.Millisecond, "no call on a failed check")
		require.NoError(t, pubErr)
		mu.Lock()
		assert.Equal(t, "e", seen[0])
		mu.Unlock()
		assert.Contains(t, buf.String(), "Error checking cancel mark of e")
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
			assert.False(t, marked(t, c.cons, "m"))
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
		assert.False(t, marked(t, c.cons, "n"))
		v, ok, _ := ackPopOf(c.cons)
		require.True(t, ok)
		assert.Equal(t, "n", envID(t, v))

		require.NoError(t, c.prod.CancelJob("p"))
		require.True(t, c.pushOnce(envelope("p", `1`), "p"))
		require.NoError(t, c.prod.CancelJob("p"))
		require.True(t, c.pushOnce(envelope("p", `2`), "p")) // dedupe hit: mark stays
		assert.True(t, marked(t, c.cons, "p"))

		c.prod.Purge()
		assert.Zero(t, c.cons.client.Exists(ctx, c.cons.getCancelledKey()).Val())
	})
}

// A plain Enqueue keeps the id's mark, so the pop drops the job.
func TestCancelPlainEnqueueDropped(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		require.NoError(t, c.prod.CancelJob("n"))
		require.True(t, c.push(envelope("n", `1`), 1))
		_, ok, _ := ackPopOf(c.cons)
		assert.False(t, ok)
		assertDropped(t, c.cons, "n")
	})
}

// A cancelled running job whose consumer dies is requeued, and the next pop
// drops it, its attempts counted.
func TestCancelRequeuedRunningJob(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		got, _ := watch(c.prod, "r")
		require.True(t, c.push(envelope("r", `1`), 1))
		_, ok, _ := ackPopOf(c.cons)
		require.True(t, ok)
		require.NoError(t, c.prod.CancelJob("r"))
		require.NoError(t, c.cons.Close()) // requeues its job

		_, ok, _ = ackPopOf(c.prod)
		assert.False(t, ok, "dropped")
		r := record(t, c.prod, "r")
		assert.Equal(t, "cancelled", r["state"])
		assert.EqualValues(t, 2, r["attempts"])
		assert.NotContains(t, r, "started_at")
		assert.False(t, marked(t, c.prod, "r"))
		assert.Equal(t, contract.JobOutcome{ID: "r", State: contract.JobCancelled, Error: "cancelled"}, outcomeWithin(t, got, time.Second))
	})
}

func TestCancelJobClosedQueue(t *testing.T) {
	q, cleanup := setupTestQueue(t)
	defer cleanup()
	assert.ErrorIs(t, q.CancelJob(""), errEmptyJobID)
	require.NoError(t, q.Close())
	assert.ErrorIs(t, q.CancelJob("x"), errQueueClosed)
}

// With the dedupe off, a pushing EnqueueOnce still drops the id's cancel
// mark and old record (spec 6.13, 6.14 Re-add).
func TestEnqueueOnceNoWindowReAdd(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		c.prod.SetDedupWindow(0)
		require.NoError(t, c.prod.CancelJob("m"))
		require.True(t, c.pushOnce(envelope("m", `1`), "m"))
		assert.False(t, marked(t, c.cons, "m"), "the re-add drops the mark")
		v, ok, ackID := ackPopOf(c.cons)
		require.True(t, ok, "the re-added job is not dropped")
		assert.Equal(t, "m", envID(t, v))
		require.True(t, c.cons.Acknowledge(ackID))

		require.True(t, c.pushOnce(envelope("m", `2`), "m")) // re-add of a completed id
		assert.Zero(t, c.prod.client.Exists(t.Context(), c.prod.queueKey+":dedup:m").Val(), "no id key")
		assert.False(t, c.cons.client.HExists(t.Context(), c.cons.getJobsKey(), "m").Val(), "old record gone")
		got, _ := watch(c.prod, "m")
		c.prod.catchUp([]string{"m"})
		require.NoError(t, c.prod.CancelJob("m"), "the waiting job is not ErrJobEnded")
		_, ok, _ = ackPopOf(c.cons)
		assert.False(t, ok, "dropped")
		assert.Equal(t, contract.JobOutcome{ID: "m", State: contract.JobCancelled, Error: "cancelled"},
			outcomeWithin(t, got, time.Second), "the old run's outcome is not reported")
	})
}

// An expired mark counts as none: the pop hands the item out and removes
// the mark. CancelJob and the sweep prune expired marks.
func TestCancelExpiredMark(t *testing.T) {
	eachCancelCase(t, func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		key := c.cons.getCancelledKey()
		past := float64(c.cons.client.Time(ctx).Val().UnixMilli() - 1000)
		require.NoError(t, c.cons.client.ZAdd(ctx, key, redis.Z{Score: past, Member: "e"}).Err())
		require.True(t, c.push(envelope("e", `1`), 1))
		v, ok, _ := ackPopOf(c.cons)
		require.True(t, ok)
		assert.Equal(t, "e", envID(t, v))
		assert.False(t, c.cons.client.HExists(ctx, c.cons.getJobsKey(), "e").Val(), "no cancelled record")
		assert.Zero(t, c.cons.client.Exists(ctx, key).Val(), "the pop removed the expired mark")

		require.NoError(t, c.cons.client.ZAdd(ctx, key, redis.Z{Score: past, Member: "old"}).Err())
		require.NoError(t, c.prod.CancelJob("new"))
		assert.Equal(t, []string{"new"}, c.cons.client.ZRange(ctx, key, 0, -1).Val(), "CancelJob prunes")

		require.NoError(t, c.cons.client.ZAdd(ctx, key, redis.Z{Score: past, Member: "old"}).Err())
		assert.Eventually(t, func() bool {
			return c.cons.client.ZScore(ctx, key, "old").Err() == redis.Nil
		}, 3*time.Second, 50*time.Millisecond, "the sweep prunes")
		assert.True(t, marked(t, c.cons, "new"), "a live mark stays")
	})
}

// A pop that drops marked items trims :failed to the failed retention at
// once, not at the next minute's sweep trim.
func TestCancelDropTrimsFailed(t *testing.T) {
	pops := map[string]func(q *Queue){
		"raw":       func(q *Queue) { rawPopOf(q) },
		"single":    func(q *Queue) { ackPopOf(q) },
		"batch one": func(q *Queue) { batchPopOf(q, make([]any, 1), make([]string, 1)) }, // the lean batch script
		"batch": func(q *Queue) { // want 2: the full batch script
			q.popLen.Store(2)
			batchPopOf(q, make([]any, 2), make([]string, 2))
		},
	}
	for name, pop := range pops {
		t.Run(name, func(t *testing.T) {
			eachCancelCase(t, func(t *testing.T, c cancelQueues) {
				ctx := t.Context()
				c.cons.SetFailedRetention(2, time.Hour)
				for i := range 5 {
					id := fmt.Sprintf("c%d", i)
					require.True(t, c.push(envelope(id, `1`), 1))
					require.NoError(t, c.prod.CancelJob(id))
				}
				require.True(t, c.push(envelope("live", `1`), 1))

				pop(c.cons)
				assert.EqualValues(t, 2, c.cons.client.ZCard(ctx, c.cons.getFailedKey()).Val())
				assert.EqualValues(t, 2, c.cons.client.HLen(ctx, c.cons.getJobsKey()).Val())
			})
		})
	}
}

// One script call drops at most maxPop marked items; the script that ran
// out of budget leaves the rest and keeps the consumer out of :idle, and
// every pop call runs it again.
func TestCancelDropBudget(t *testing.T) {
	const marks = maxPop + 500
	fill := func(t *testing.T, c cancelQueues) {
		ctx := t.Context()
		c.cons.Subscribe(func(string) {})
		exp := float64(c.cons.client.Time(ctx).Val().UnixMilli() + time.Hour.Milliseconds())
		z := make([]redis.Z, marks)
		for i := range marks {
			id := fmt.Sprintf("c%d", i)
			require.True(t, c.push(envelope(id, `1`), 1))
			z[i] = redis.Z{Score: exp, Member: id}
		}
		require.True(t, c.push(envelope("live", `1`), 1))
		require.NoError(t, c.cons.client.ZAdd(ctx, c.cons.getCancelledKey(), z...).Err())
	}
	check := func(t *testing.T, q *Queue, dropped int64, idle bool) {
		t.Helper()
		ctx := t.Context()
		assert.Equal(t, dropped, q.client.ZCard(ctx, q.getFailedKey()).Val())
		assert.Equal(t, dropped, q.client.HLen(ctx, q.getJobsKey()).Val())
		assert.Equal(t, idle, q.client.SIsMember(ctx, q.getIdleKey(), q.consumerID()).Val(), "idle")
	}
	extra := func(q *Queue) []string {
		if q.isPriority {
			return []string{q.getPriorityKey()}
		}
		return nil
	}

	// one batch call pops again after a spent budget, with the lean (want 1)
	// and the full (want 2) script
	for _, want := range []int64{1, 2} {
		t.Run(fmt.Sprintf("batch want %d", want), func(t *testing.T) {
			eachCancelCase(t, func(t *testing.T, c cancelQueues) {
				fill(t, c)
				c.cons.popLen.Store(want)
				items, ids := make([]any, 2), make([]string, 2)
				n, remaining, err := batchPopOf(c.cons, items, ids)
				require.NoError(t, err)
				require.Equal(t, 1, n)
				assert.Zero(t, remaining)
				assert.Equal(t, "live", envID(t, items[0]))
				check(t, c.cons, marks, true) // short of its slots with the queue empty
				assert.Equal(t, "cancelled", record(t, c.cons, "c0")["state"])
				assert.Equal(t, "cancelled", record(t, c.cons, fmt.Sprintf("c%d", marks-1))["state"])
			})
		})
	}
	t.Run("single", func(t *testing.T) {
		eachCancelCase(t, func(t *testing.T, c cancelQueues) {
			fill(t, c)
			q := c.cons
			pop := listAckPopOne
			if q.isPriority {
				pop = zsetAckPopOne
			}
			ackID := q.ackIDOf(fmt.Sprintf("%020d", q.ackSeq.Add(1)), 1)
			args := append([]any{q.vt(), ackID, q.consumerID()}, append(q.cancelArgs(), q.hbt(), q.ackPrefix)...)
			r, err := pop.Run(t.Context(), q.client, q.popKeys(extra(q)...), args...).Result()
			require.NoError(t, err)
			assert.EqualValues(t, -1, r, "budget spent")
			check(t, q, maxPop, false)

			v, ok, _ := ackPopOf(q)
			require.True(t, ok)
			assert.Equal(t, "live", envID(t, v))
			check(t, q, marks, false)
		})
	})
	t.Run("single, one call", func(t *testing.T) {
		eachCancelCase(t, func(t *testing.T, c cancelQueues) {
			fill(t, c)
			v, ok, _ := ackPopOf(c.cons) // pops again after a spent budget
			require.True(t, ok)
			assert.Equal(t, "live", envID(t, v))
			check(t, c.cons, marks, false)
		})
	})
	t.Run("raw", func(t *testing.T) {
		eachCancelCase(t, func(t *testing.T, c cancelQueues) {
			fill(t, c)
			v, ok := rawPopOf(c.cons) // pops again after a spent budget
			require.True(t, ok)
			assert.Equal(t, "live", envID(t, v))
			check(t, c.cons, marks, false)
		})
	})
}

// marked reports whether q's cancel marks hold id.
func marked(t *testing.T, q *Queue, id string) bool {
	t.Helper()
	err := q.client.ZScore(t.Context(), q.getCancelledKey(), id).Err()
	if err == redis.Nil {
		return false
	}
	require.NoError(t, err)
	return true
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

// outcomeCase opens a producer and a consumer object of one queue kind on
// key, as two processes would.
type outcomeCase struct {
	name string
	open func(qs Queues, key string) (prod, cons *Queue, push func(item string) bool)
}

var outcomeCases = []outcomeCase{
	{"list", func(qs Queues, key string) (*Queue, *Queue, func(string) bool) {
		p, c := qs.NewQueue(key), qs.NewQueue(key)
		return p, c, func(v string) bool { return p.Enqueue(v) }
	}},
	{"priority", func(qs Queues, key string) (*Queue, *Queue, func(string) bool) {
		p, c := qs.NewPriorityQueue(key), qs.NewPriorityQueue(key)
		return p.Queue, c.Queue, func(v string) bool { return p.Enqueue(v, 1) }
	}},
}

// eachOutcomeCase runs fn for both kinds on a fresh key.
func eachOutcomeCase(t *testing.T, fn func(t *testing.T, prod, cons *Queue, push func(string) bool)) {
	for _, c := range outcomeCases {
		t.Run(c.name, func(t *testing.T) {
			qs := New(getTestRedisURL())
			defer qs.Close()
			prod, cons, push := c.open(qs, newID())
			defer prod.Close()
			defer cons.Close()
			fn(t, prod, cons, push)
		})
	}
}

// popAck pops the one waiting item from q and returns its ack ID.
func popAck(t *testing.T, q *Queue) string {
	t.Helper()
	var ackID string
	require.Eventually(t, func() bool {
		var ok bool
		if q.isPriority {
			_, ok, ackID = (&PriorityQueue{q}).DequeueWithAckId()
		} else {
			_, ok, ackID = q.DequeueWithAckId()
		}
		return ok
	}, 2*time.Second, 10*time.Millisecond)
	return ackID
}

// watch watches id on q and returns the channel fn sends each call to.
func watch(q *Queue, id string) (<-chan contract.JobOutcome, func()) {
	got := make(chan contract.JobOutcome, 4)
	cancel := q.WatchOutcome(id, func(o contract.JobOutcome) { got <- o })
	return got, cancel
}

func outcomeWithin(t *testing.T, got <-chan contract.JobOutcome, d time.Duration) contract.JobOutcome {
	t.Helper()
	select {
	case o := <-got:
		return o
	case <-time.After(d):
		t.Fatal("no outcome")
		return contract.JobOutcome{}
	}
}

// watchGone asserts q no longer watches id: no entry, no subscription.
func watchGone(t *testing.T, q *Queue, id string) {
	t.Helper()
	assert.Eventually(t, func() bool {
		q.outcomes.mu.Lock()
		_, held := q.outcomes.byID[id]
		q.outcomes.mu.Unlock()
		n, err := q.client.PubSubNumSub(context.Background(), q.donePrefix()+id).Result()
		return !held && err == nil && n[q.donePrefix()+id] == 0
	}, 2*time.Second, 20*time.Millisecond, "watch of %s still held", id)
}

func TestWatchOutcomeAcknowledge(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, _ := watch(prod, "j1")
		require.True(t, push(envelope("j1", `{"a":1}`)))
		require.True(t, cons.Acknowledge(popAck(t, cons)))

		o := outcomeWithin(t, got, time.Second) // a message, well before the sweep
		assert.Equal(t, contract.JobOutcome{ID: "j1", State: contract.JobCompleted}, o)
		watchGone(t, prod, "j1") // the watch ends by itself after fn
	})
}

func TestWatchOutcomeReject(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, _ := watch(prod, "j2")
		require.True(t, push(envelope("j2", `1`)))
		require.True(t, cons.Reject(popAck(t, cons), "boom"))

		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, contract.JobOutcome{ID: "j2", State: contract.JobFailed, Error: "boom"}, o)
	})
}

// A result keeps its exact bytes, big integers included, in the outcome
// and the record.
func TestAcknowledgeResultKeepsBytes(t *testing.T) {
	const result = `{"n":12345678901234567}`
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, _ := watch(prod, "j3")
		require.True(t, push(envelope("j3", `"x"`)))
		require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(result)))

		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, contract.JobCompleted, o.State)
		assert.Equal(t, result, string(o.Result))

		rec, err := cons.client.HGet(t.Context(), cons.getJobsKey(), "j3").Result()
		require.NoError(t, err)
		assert.Contains(t, rec, `"result":`+result)
		out, ok := outcomeOf("j3", rec)
		require.True(t, ok)
		assert.Equal(t, result, string(out.Result))

		s, err := cons.QueueStats(t.Context())
		require.NoError(t, err)
		assert.EqualValues(t, 1, s.Totals.Completed)
	})
}

// Invalid JSON is refused and leaves the job processing; the script's own
// check refuses what cjson can't decode before any write.
func TestAcknowledgeResultInvalidJSON(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		require.True(t, push(envelope("j4", `1`)))
		ackID := popAck(t, cons)
		assert.False(t, cons.AcknowledgeResult(ackID, []byte(`{"n":`)))
		assert.False(t, cons.AcknowledgeResult(ackID, nil))
		assert.Equal(t, 1, cons.GetNackedItemsCount(), "still processing")

		keys := []string{cons.getNackedItemKey(), cons.getTimeoutKey(), cons.getStartedKey(), cons.getAttemptsKey(), cons.getStatsKey(), cons.getJobsKey(), cons.getCompletedKey(), cons.getFailedKey(), cons.getPriorityKey()}
		r, err := finishScript.Run(t.Context(), cons.client, keys, ackID, "completed", "", 10, 60000, `{bad`, cons.donePrefix()).Int()
		require.NoError(t, err)
		assert.Equal(t, -1, r)
		assert.Equal(t, 1, cons.GetNackedItemsCount(), "still processing")
		assert.Zero(t, cons.client.HLen(t.Context(), cons.getJobsKey()).Val(), "no record")

		assert.True(t, cons.AcknowledgeResult(ackID, []byte(` [1, 2] `)))
		assert.Zero(t, cons.GetNackedItemsCount())
	})
}

// A job that ended before the watch is reported from its record.
func TestWatchOutcomeCatchUp(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		require.True(t, push(envelope("j5", `1`)))
		require.True(t, cons.Reject(popAck(t, cons), "late"))

		got, _ := watch(prod, "j5")
		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, contract.JobOutcome{ID: "j5", State: contract.JobFailed, Error: "late"}, o)
		watchGone(t, prod, "j5")
	})
}

// A lost message is made up by the sweep's read of the record.
func TestWatchOutcomeSweepCatchUp(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got := make(chan contract.JobOutcome, 1)
		addWatch(prod, "j6", func(o contract.JobOutcome) { got <- o }) // no subscribe, no catch-up
		rec := `{"id":"j6","state":"completed","item":1,"finished_at":1,"attempts":1,"result":7}`
		require.NoError(t, cons.client.HSet(t.Context(), cons.getJobsKey(), "j6", rec).Err()) // no PUBLISH

		prod.sweepOutcomes()
		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, `7`, string(o.Result))
		watchGone(t, prod, "j6")

		addWatch(prod, "j6b", func(o contract.JobOutcome) { got <- o }) // left to the 1s sweep loop
		rec = `{"id":"j6b","state":"failed","item":1,"finished_at":1,"attempts":1}`
		require.NoError(t, cons.client.HSet(t.Context(), cons.getJobsKey(), "j6b", rec).Err())
		assert.Equal(t, contract.JobOutcome{ID: "j6b", State: contract.JobFailed}, outcomeWithin(t, got, 3*time.Second))
	})
}

// addWatch adds a watch of id to q as WatchOutcome does, but without its
// subscribe and catch-up, so only a sweep (or a message) delivers it.
func addWatch(q *Queue, id string, fn func(contract.JobOutcome)) {
	o := &q.outcomes
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.byID == nil {
		o.byID = map[string]map[*outcomeWatch]struct{}{}
	}
	if o.byID[id] == nil {
		o.byID[id] = map[*outcomeWatch]struct{}{}
	}
	o.byID[id][&outcomeWatch{fn: fn}] = struct{}{}
}

// One sweep reads at most maxCatchUp watched ids; the next takes the rest.
// A queue object without its sweep, so only these calls deliver.
func TestSweepOutcomesCap(t *testing.T) {
	qs := New(getTestRedisURL()).(*queues)
	defer qs.Close()
	q := newQueueBase(qs.client, "redisq:test:{"+newID()+"}")
	defer q.cancel()
	q.presence = q.client.Subscribe(q.ctx)
	defer q.presence.Close()
	defer q.client.Del(context.Background(), q.getJobsKey())

	q.unwatch("none", &outcomeWatch{}) // no set: nothing to drop
	const n = maxCatchUp + 100
	var calls atomic.Int32
	fields := make([]any, 0, 2*n)
	for i := range n {
		id := fmt.Sprintf("s%d", i)
		fields = append(fields, id, `{"state":"completed"}`)
		addWatch(q, id, func(contract.JobOutcome) { calls.Add(1) })
	}
	require.NoError(t, q.client.HSet(t.Context(), q.getJobsKey(), fields...).Err())

	q.sweepOutcomes()
	assert.Len(t, q.outcomes.byID, n-maxCatchUp, "one sweep reads maxCatchUp ids")
	q.sweepOutcomes()
	assert.Empty(t, q.outcomes.byID)
	assert.Eventually(t, func() bool { return calls.Load() == n }, time.Second, 10*time.Millisecond)
}

// A record that isn't JSON or isn't final gives no outcome; a cancelled one
// without errors gets error "cancelled".
func TestOutcomeOf(t *testing.T) {
	for _, rec := range []string{`{`, `[]`, `{"state":"processing"}`, `{"state":"pending"}`} {
		_, ok := outcomeOf("x", rec)
		assert.False(t, ok, rec)
	}
	out, ok := outcomeOf("x", `{"state":"cancelled"}`)
	require.True(t, ok)
	assert.Equal(t, contract.JobOutcome{ID: "x", State: contract.JobCancelled, Error: "cancelled"}, out)
}

// A done message that isn't JSON or isn't final calls nothing; the id comes
// from the channel, not the payload.
func TestWatchOutcomeIgnoresBadPayload(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		ctx := t.Context()
		ch := prod.donePrefix() + "j"
		got, _ := watch(prod, "j")
		require.Eventually(t, func() bool {
			n, err := cons.client.PubSubNumSub(ctx, ch).Result()
			return err == nil && n[ch] == 1
		}, 2*time.Second, 10*time.Millisecond, "subscribed")

		for _, p := range []string{`{`, `{"state":"processing"}`, `{"id":"other","state":"completed","result":1}`} {
			require.NoError(t, cons.client.Publish(ctx, ch, p).Err())
		}
		assert.Equal(t, contract.JobOutcome{ID: "j", State: contract.JobCompleted, Result: json.RawMessage(`1`)}, outcomeWithin(t, got, time.Second))
		watchGone(t, prod, "j")
	})
}

func TestWatchOutcomeCancel(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, cancel := watch(prod, "j7")
		cancel()
		cancel() // idempotent
		watchGone(t, prod, "j7")
		require.True(t, push(envelope("j7", `1`)))
		require.True(t, cons.Acknowledge(popAck(t, cons)))

		select {
		case <-got:
			t.Fatal("fn called after cancel")
		case <-time.After(1500 * time.Millisecond): // past a sweep
		}
	})
}

func TestWatchOutcomeTwoWatchers(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		var a, b atomic.Int32
		cancelA := prod.WatchOutcome("j8", func(contract.JobOutcome) { a.Add(1) })
		prod.WatchOutcome("j8", func(contract.JobOutcome) { b.Add(1) })
		require.True(t, push(envelope("j8", `1`)))
		require.True(t, cons.Acknowledge(popAck(t, cons)))

		require.Eventually(t, func() bool { return a.Load() == 1 && b.Load() == 1 }, time.Second, 10*time.Millisecond)
		cancelA()                           // after fn: no-op
		time.Sleep(1500 * time.Millisecond) // a sweep reads the record again
		assert.EqualValues(t, 1, a.Load())
		assert.EqualValues(t, 1, b.Load())
		watchGone(t, prod, "j8")
	})
}

// Cancelling one of two watchers before the job ends keeps the other's
// entry and subscription: it still gets the outcome by message.
func TestWatchOutcomeOneOfTwoCancelled(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		var a, b atomic.Int32
		cancelA := prod.WatchOutcome("j9", func(contract.JobOutcome) { a.Add(1) })
		prod.WatchOutcome("j9", func(contract.JobOutcome) { b.Add(1) })
		cancelA()

		prod.outcomes.mu.Lock()
		_, held := prod.outcomes.byID["j9"]
		prod.outcomes.mu.Unlock()
		assert.True(t, held, "B's entry stays")
		ch := prod.donePrefix() + "j9"
		require.Eventually(t, func() bool { // SUBSCRIBE is not awaited
			return prod.client.PubSubNumSub(t.Context(), ch).Val()[ch] == 1
		}, time.Second, 10*time.Millisecond, "still subscribed")

		require.True(t, push(envelope("j9", `1`)))
		require.True(t, cons.Acknowledge(popAck(t, cons)))
		require.Eventually(t, func() bool { return b.Load() == 1 }, time.Second, 10*time.Millisecond)
		time.Sleep(1500 * time.Millisecond) // past a sweep
		assert.Zero(t, a.Load(), "A cancelled")
		assert.EqualValues(t, 1, b.Load())
		watchGone(t, prod, "j9")
	})
}

// A job without an envelope id publishes nothing; an empty id watches
// nothing.
func TestOutcomeWithoutJobID(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		ps := cons.client.PSubscribe(t.Context(), cons.donePrefix()+"*")
		defer ps.Close()
		_, err := ps.Receive(t.Context()) // subscribed
		require.NoError(t, err)

		called := make(chan struct{}, 1)
		prod.WatchOutcome("", func(contract.JobOutcome) { called <- struct{}{} })()

		require.True(t, push("raw bytes"))
		require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(`1`)))
		require.True(t, push(`{"id":7}`)) // id not a string
		require.True(t, cons.Reject(popAck(t, cons), "x"))

		select {
		case m := <-ps.Channel():
			t.Fatalf("published %s", m.Channel)
		case <-called:
			t.Fatal("fn called for an empty id")
		case <-time.After(300 * time.Millisecond):
		}
	})
}

// Retention that keeps no record still publishes the outcome.
func TestOutcomePublishedWithoutRecord(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		cons.SetCompletedRetention(0, 0)
		got, _ := watch(prod, "j9")
		require.True(t, push(envelope("j9", `1`)))
		require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(`"r"`)))

		o := outcomeWithin(t, got, time.Second)
		assert.Equal(t, `"r"`, string(o.Result))
		assert.Zero(t, cons.client.HLen(t.Context(), cons.getJobsKey()).Val())
	})
}

// Close drops watchers without calling them; a watch after Close is a
// no-op.
func TestWatchOutcomeClose(t *testing.T) {
	eachOutcomeCase(t, func(t *testing.T, prod, cons *Queue, push func(string) bool) {
		got, cancel := watch(prod, "j10")
		require.NoError(t, prod.Close())
		cancel()
		late, _ := watch(prod, "j10")

		rec := `{"id":"j10","state":"completed","item":1,"finished_at":1,"attempts":1}`
		require.NoError(t, cons.client.HSet(t.Context(), cons.getJobsKey(), "j10", rec).Err())
		require.NoError(t, cons.client.Publish(t.Context(), cons.donePrefix()+"j10", `{"id":"j10","state":"completed"}`).Err())
		select {
		case <-got:
			t.Fatal("fn called after Close")
		case <-late:
			t.Fatal("fn of a watch made after Close called")
		case <-time.After(1500 * time.Millisecond):
		}
	})
}

// An id pushed again after its dedupe window drops the previous run's
// record, so a watch of the new run waits for it; a dedupe hit keeps it.
func TestEnqueueOnceDropsOldRecord(t *testing.T) {
	for _, c := range []struct {
		name string
		open func(qs Queues, key string) (prod, cons *Queue, pushOnce func(item, id string) bool)
	}{
		{"list", func(qs Queues, key string) (*Queue, *Queue, func(string, string) bool) {
			p, c := qs.NewQueue(key), qs.NewQueue(key)
			return p, c, func(v, id string) bool { return p.EnqueueOnce(v, id) }
		}},
		{"priority", func(qs Queues, key string) (*Queue, *Queue, func(string, string) bool) {
			p, c := qs.NewPriorityQueue(key), qs.NewPriorityQueue(key)
			return p.Queue, c.Queue, func(v, id string) bool { return p.EnqueueOnce(v, 1, id) }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			qs := New(getTestRedisURL())
			defer qs.Close()
			prod, cons, pushOnce := c.open(qs, newID())
			defer prod.Close()
			defer cons.Close()
			ctx := t.Context()
			record := func() bool { return cons.client.HExists(ctx, cons.getJobsKey(), "x").Val() }
			finished := func() int64 {
				return cons.client.ZCard(ctx, cons.getCompletedKey()).Val() + cons.client.ZCard(ctx, cons.getFailedKey()).Val()
			}

			require.True(t, pushOnce(envelope("x", `1`), "x"))
			require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(`"old"`)))
			require.True(t, record())

			require.True(t, pushOnce(envelope("x", `2`), "x")) // dedupe hit
			assert.True(t, record(), "a dedupe hit keeps the record")
			assert.EqualValues(t, 1, finished())

			require.NoError(t, cons.client.Del(ctx, cons.queueKey+":dedup:x").Err()) // the window passed
			require.True(t, pushOnce(envelope("x", `3`), "x"))
			assert.False(t, record(), "the push drops the old record")
			assert.Zero(t, finished())

			got, _ := watch(prod, "x")
			select {
			case o := <-got:
				t.Fatalf("stale outcome %+v", o)
			case <-time.After(1500 * time.Millisecond): // past a sweep
			}

			require.True(t, cons.AcknowledgeResult(popAck(t, cons), []byte(`"new"`)))
			o := outcomeWithin(t, got, time.Second)
			assert.Equal(t, `"new"`, string(o.Result))
		})
	}
}

// wfTest is three queues of one fresh namespace and its workflow store:
// a (fifo), b (prio) and c (fifo).
type wfTest struct {
	t     *testing.T
	qs    Queues
	store *WorkflowStore
	a, c  *Queue
	b     *PriorityQueue
}

func newWFTest(t *testing.T, opts ...WorkflowOption) *wfTest {
	qs := New(getTestRedisURL(), WithNamespace(newID()))
	store, err := NewWorkflowStore(qs, opts...)
	require.NoError(t, err)
	e := &wfTest{t: t, qs: qs, store: store, a: qs.NewQueue("a"), b: qs.NewPriorityQueue("b"), c: qs.NewQueue("c")}
	t.Cleanup(func() {
		e.a.Close()
		e.b.Close()
		e.c.Close()
		store.Close()
		qs.Close()
	})
	return e
}

func (e *wfTest) all() []*Queue { return []*Queue{e.a, e.b.Queue, e.c} }

// tk is one task of a test plan: queue q (*Queue or *PriorityQueue), deps,
// policy, priority and an undo queue (nil: none). data is its JSON data,
// the name as a string when empty.
type tk struct {
	name   string
	q      any
	deps   []string
	always bool
	prio   int
	undo   any
	data   string
}

func idOf(q any) string {
	if pq, ok := q.(*PriorityQueue); ok {
		return pq.queueKey
	}
	return q.(*Queue).queueKey
}

// wfEnvelope is the envelope varmq writes for task (undo: its undo).
func wfEnvelope(wf, task string, undo bool, data string) []byte {
	id, u := "wf:"+wf+":"+task, ""
	if undo {
		id, u = id+":undo", `,"undo":true`
	}
	return fmt.Appendf(nil, `{"id":%q,"status":"Created","data":%s,"enqueued_at":1762228800000,"wf":%q,"task":%q%s}`, id, data, wf, task, u)
}

func wfPlan(id string, failFast, undoOnCancel bool, tasks ...tk) contract.WorkflowPlan {
	p := contract.WorkflowPlan{ID: id, FailFast: failFast, UndoOnCancel: undoOnCancel, Hash: "hash-" + id}
	for _, x := range tasks {
		data := x.data
		if data == "" {
			data = strconv.Quote(x.name)
		}
		jid := "wf:" + id + ":" + x.name
		tp := contract.TaskPlan{Name: x.name, JobID: jid, QueueID: idOf(x.q), Queue: x.q, Priority: x.prio,
			HasPriority: x.prio != 0, Deps: x.deps, Always: x.always, Item: wfEnvelope(id, x.name, false, data)}
		if x.undo != nil {
			tp.Undo = &contract.UndoPlan{JobID: jid + ":undo", QueueID: idOf(x.undo), Queue: x.undo,
				Item: wfEnvelope(id, x.name, true, `{"big":12345678901234567890}`)}
		}
		p.Tasks = append(p.Tasks, tp)
	}
	return p
}

// wfPopped is one workflow item a test consumer took.
type wfPopped struct {
	q        *Queue
	ack      string
	raw      []byte
	wf, task string
	undo     bool
}

func wfPop(t *testing.T, q *Queue) (wfPopped, bool) {
	t.Helper()
	var v any
	var ok bool
	var ack string
	if q.isPriority {
		v, ok, ack = (&PriorityQueue{q}).DequeueWithAckId()
	} else {
		v, ok, ack = q.DequeueWithAckId()
	}
	if !ok {
		return wfPopped{}, false
	}
	var env struct {
		WF   string `json:"wf"`
		Task string `json:"task"`
		Undo bool   `json:"undo"`
	}
	require.NoError(t, json.Unmarshal(v.([]byte), &env))
	return wfPopped{q, ack, v.([]byte), env.WF, env.Task, env.Undo}, true
}

// label is "task" or "task:undo".
func (p wfPopped) label() string {
	if p.undo {
		return p.task + ":undo"
	}
	return p.task
}

// finish ends p as a varmq worker does: pre-run check first (not runnable:
// cancelled, unrun), else run's outcome. It reports whether p ran.
func (e *wfTest) finish(p wfPopped, run func(p wfPopped) (contract.Outcome, []byte)) bool {
	e.t.Helper()
	end := contract.TaskEnd{Workflow: p.wf, Task: p.task, Undo: p.undo, Outcome: contract.OutcomeCancelled, Reason: "workflow cancelled"}
	ok, err := e.store.Runnable(e.t.Context(), p.wf, p.task, p.undo)
	if !errors.Is(err, contract.ErrWorkflowNotFound) {
		require.NoError(e.t, err)
	}
	if ok {
		end.Outcome, end.Result = run(p)
		end.Reason = ""
		if end.Outcome != contract.OutcomeCompleted {
			end.Reason = p.label() + " broke"
		}
	}
	_, done := e.store.FinishTask(p.q.QueueID(), p.ack, end)
	require.True(e.t, done)
	return ok
}

// drain pops and finishes every item of the test queues until they stay
// empty; it returns the labels of the items that ran, in run order.
func (e *wfTest) drain(run func(p wfPopped) (contract.Outcome, []byte)) []string {
	e.t.Helper()
	var ran []string
	for more := true; more; {
		more = false
		for _, q := range e.all() {
			for p, ok := wfPop(e.t, q); ok; p, ok = wfPop(e.t, q) {
				more = true
				if e.finish(p, run) {
					ran = append(ran, p.label())
				}
			}
		}
	}
	return ran
}

// completeAll completes every task, its result `"<label>"`.
func completeAll(p wfPopped) (contract.Outcome, []byte) {
	return contract.OutcomeCompleted, []byte(strconv.Quote(p.label()))
}

// failing completes every task but those named, which fail.
func failing(names ...string) func(wfPopped) (contract.Outcome, []byte) {
	return func(p wfPopped) (contract.Outcome, []byte) {
		if slices.Contains(names, p.label()) {
			return contract.OutcomeFailed, nil
		}
		return completeAll(p)
	}
}

func (e *wfTest) record(id string) contract.WorkflowRecord {
	e.t.Helper()
	r, err := e.store.Record(e.t.Context(), id)
	require.NoError(e.t, err)
	return r
}

func taskStates(r contract.WorkflowRecord) map[string]contract.JobState {
	m := map[string]contract.JobState{}
	for _, t := range r.Tasks {
		m[t.Name] = t.State
		if t.Undo != nil {
			m[t.Name+":undo"] = t.Undo.State
		}
	}
	return m
}

// A DAG across a fifo, a priority and another fifo queue runs in
// dependency order; results, records, items and counters are as submitted.
func TestWorkflowDAGAcrossQueues(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	p := wfPlan("dag", false, true,
		tk{name: "a", q: e.a, data: `{"n":12345678901234567890}`},
		tk{name: "b", q: e.b, deps: []string{"a"}, prio: 3},
		tk{name: "c", q: e.c, deps: []string{"a"}},
		tk{name: "d", q: e.a, deps: []string{"b", "c"}},
	)
	require.NoError(t, e.store.Submit(ctx, p))
	require.NoError(t, e.store.Submit(ctx, p), "same plan again: success")
	assert.EqualValues(t, 1, e.a.client.LLen(ctx, e.a.queueKey).Val(), "root pushed once")

	q := p
	q.Hash = "other"
	assert.ErrorIs(t, e.store.Submit(ctx, q), contract.ErrWorkflowExists)

	// The root's item is the submitted bytes, and nothing else is queued.
	a, ok := wfPop(t, e.a)
	require.True(t, ok)
	assert.Equal(t, string(p.Tasks[0].Item.([]byte)), string(a.raw))
	for _, q := range []*Queue{e.b.Queue, e.c} {
		_, ok := wfPop(t, q)
		assert.False(t, ok, "dependents wait in the workflow")
	}
	run, err := e.store.Runnable(ctx, "dag", "a", false)
	require.NoError(t, err)
	assert.True(t, run)
	pushed, ok := e.store.FinishTask(e.a.QueueID(), a.ack, contract.TaskEnd{Workflow: "dag", Task: "a", Outcome: contract.OutcomeCompleted, Result: []byte(`{"url":"s3://x"}`)})
	require.True(t, ok)
	assert.Equal(t, []string{e.b.queueKey, e.c.queueKey}, pushed)

	res, err := e.store.DepResult(ctx, "dag", "a")
	require.NoError(t, err)
	assert.JSONEq(t, `{"url":"s3://x"}`, string(res))
	_, err = e.store.DepResult(ctx, "dag", "b")
	assert.ErrorIs(t, err, contract.ErrNoResult, "not ended yet")

	ran := e.drain(completeAll)
	assert.Equal(t, []string{"b", "c", "d"}, ran)

	r := e.record("dag")
	assert.Equal(t, contract.WorkflowCompleted, r.State)
	assert.NotZero(t, r.CreatedAt)
	assert.GreaterOrEqual(t, r.FinishedAt, r.CreatedAt)
	assert.True(t, r.UndoOnCancel)
	require.Len(t, r.Tasks, 4)
	d := r.Tasks[3]
	assert.NotZero(t, d.EnqueuedAt)
	assert.GreaterOrEqual(t, d.StartedAt, d.EnqueuedAt)
	assert.GreaterOrEqual(t, d.FinishedAt, d.StartedAt)
	d.EnqueuedAt, d.StartedAt, d.FinishedAt = 0, 0, 0 // clock values, checked above
	assert.Equal(t, contract.TaskRecord{Name: "d", State: contract.JobCompleted, Deps: []string{"b", "c"},
		QueueID: e.a.queueKey, JobID: "wf:dag:d", ResultSize: len(`"d"`)}, d)
	assert.Equal(t, e.b.queueKey, r.Tasks[1].QueueID)

	// Each task left a completed record in its queue and was counted.
	assert.Equal(t, []any{"2", "2"}, e.a.client.HMGet(ctx, e.a.getStatsKey(), "submitted", "completed").Val(), "a and d")
	rec := e.b.client.HGet(ctx, e.b.getJobsKey(), "wf:dag:b").Val()
	assert.Contains(t, rec, `"result":"b"`)

	// A late copy of an ended task settles and changes nothing.
	require.True(t, e.a.Enqueue(wfEnvelope("dag", "a", false, "1")))
	late, ok := wfPop(t, e.a)
	require.True(t, ok)
	pushed, ok = e.store.FinishTask(e.a.QueueID(), late.ack, contract.TaskEnd{Workflow: "dag", Task: "a", Outcome: contract.OutcomeFailed, Reason: "late"})
	require.True(t, ok)
	assert.Empty(t, pushed)
	assert.Equal(t, r, e.record("dag"))

	ttl := e.a.client.PTTL(ctx, e.store.key("dag")).Val()
	assert.Greater(t, ttl, 6*24*time.Hour, "ended: expires after the retention")
	_, err = e.store.Record(ctx, "nope")
	assert.ErrorIs(t, err, contract.ErrWorkflowNotFound)
}

// B fails under an Always D: absorbed, so the workflow completes and D
// sees B failed.
func TestWorkflowDiamondAlways(t *testing.T) {
	e := newWFTest(t)
	require.NoError(t, e.store.Submit(t.Context(), wfPlan("diamond", false, true,
		tk{name: "A", q: e.a},
		tk{name: "B", q: e.b, deps: []string{"A"}},
		tk{name: "C", q: e.c, deps: []string{"A"}},
		tk{name: "D", q: e.a, deps: []string{"B", "C"}, always: true},
	)))
	var bErr, cErr error
	ran := e.drain(func(p wfPopped) (contract.Outcome, []byte) {
		if p.task == "D" {
			_, bErr = e.store.DepResult(t.Context(), "diamond", "B")
			_, cErr = e.store.DepResult(t.Context(), "diamond", "C")
		}
		return failing("B")(p)
	})
	assert.ElementsMatch(t, []string{"A", "B", "C", "D"}, ran)
	assert.ErrorIs(t, bErr, contract.ErrDepFailed)
	assert.NoError(t, cErr)
	r := e.record("diamond")
	assert.Equal(t, contract.WorkflowCompleted, r.State, "B's failure is absorbed by Always D")
	assert.Equal(t, contract.JobFailed, r.Tasks[1].State)
	assert.Equal(t, "B broke", r.Tasks[1].Reason)
	assert.Equal(t, contract.JobCompleted, r.Tasks[3].State)
}

// FailFast: the first unabsorbed failure cancels every waiting task but the
// failed task's Always dependents; a queued task not yet started is
// stopped by the pre-run check.
func TestWorkflowFailFast(t *testing.T) {
	e := newWFTest(t)
	require.NoError(t, e.store.Submit(t.Context(), wfPlan("ff", true, true,
		tk{name: "a", q: e.a},
		tk{name: "b", q: e.c},
		tk{name: "c", q: e.a, deps: []string{"a"}},
		tk{name: "e", q: e.b, deps: []string{"a"}, always: true},
		tk{name: "d", q: e.c, deps: []string{"b"}},
	)))
	ran := e.drain(failing("a"))
	assert.Equal(t, []string{"a", "e"}, ran, "b was queued, stopped unrun")
	r := e.record("ff")
	assert.Equal(t, contract.WorkflowFailed, r.State)
	assert.Equal(t, map[string]contract.JobState{"a": contract.JobFailed, "b": contract.JobCancelled,
		"c": contract.JobCancelled, "e": contract.JobCompleted, "d": contract.JobCancelled}, taskStates(r))
}

// A failure compensates in saga order: b's undo first, then a's; the undo
// envelope is pushed byte for byte. A failed undo ends compensation_failed
// with its reason.
func TestWorkflowUndo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  []string
		state contract.WorkflowState
	}{
		{"compensated", []string{"c"}, contract.WorkflowCompensated},
		{"undo fails", []string{"c", "b:undo"}, contract.WorkflowCompensationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newWFTest(t)
			p := wfPlan("saga", false, true,
				tk{name: "a", q: e.a, undo: e.b},
				tk{name: "b", q: e.c, deps: []string{"a"}, undo: e.c},
				tk{name: "c", q: e.a, deps: []string{"b"}},
			)
			require.NoError(t, e.store.Submit(t.Context(), p))
			var undoRaw []byte
			ran := e.drain(func(p wfPopped) (contract.Outcome, []byte) {
				if p.label() == "a:undo" {
					undoRaw = p.raw
				}
				return failing(tc.fail...)(p)
			})
			assert.Equal(t, []string{"a", "b", "c", "b:undo", "a:undo"}, ran)
			assert.Equal(t, string(p.Tasks[0].Undo.Item.([]byte)), string(undoRaw), "undo envelope verbatim")
			r := e.record("saga")
			assert.Equal(t, tc.state, r.State)
			assert.Equal(t, idOf(e.b), r.Tasks[0].Undo.QueueID)
			assert.Equal(t, "wf:saga:a:undo", r.Tasks[0].Undo.JobID)
			if tc.state == contract.WorkflowCompensationFailed {
				assert.Equal(t, contract.JobFailed, r.Tasks[1].Undo.State)
				assert.Equal(t, "b:undo broke", r.Tasks[1].Undo.Reason)
				assert.Equal(t, contract.JobCompleted, r.Tasks[0].Undo.State, "a failed undo does not stop the others")
			}
			assert.Nil(t, r.Tasks[2].Undo)
		})
	}
}

// Cancel cancels waiting tasks at once and queued ones at their pre-run
// check; compensation follows undo_on_cancel unless Cancel overrides it.
func TestWorkflowCancel(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name  string
		undo  *bool
		state contract.WorkflowState
		ran   []string
	}{
		{"with undo", nil, contract.WorkflowCompensated, []string{"a:undo"}},
		{"without undo", &no, contract.WorkflowCancelled, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newWFTest(t)
			ctx := t.Context()
			require.NoError(t, e.store.Submit(ctx, wfPlan("cx", false, true,
				tk{name: "a", q: e.a, undo: e.c},
				tk{name: "b", q: e.b, deps: []string{"a"}},
				tk{name: "c", q: e.c},
				tk{name: "d", q: e.a, deps: []string{"b"}},
			)))
			a, ok := wfPop(t, e.a)
			require.True(t, ok)
			require.True(t, e.finish(a, completeAll))

			require.NoError(t, e.store.Cancel(ctx, "cx", tc.undo))
			require.NoError(t, e.store.Cancel(ctx, "cx", nil), "again: no change")
			r := e.record("cx")
			assert.Equal(t, contract.WorkflowRunning, r.State, "b and c still queued")
			assert.Equal(t, contract.JobCancelled, r.Tasks[3].State)
			assert.Equal(t, tc.undo == nil, r.UndoOnCancel)

			assert.Equal(t, tc.ran, e.drain(completeAll))
			r = e.record("cx")
			assert.Equal(t, tc.state, r.State)
			assert.Equal(t, contract.JobCancelled, r.Tasks[1].State)
			assert.Equal(t, "workflow cancelled", r.Tasks[1].Reason)
			assert.ErrorIs(t, e.store.Cancel(ctx, "nope", nil), contract.ErrWorkflowNotFound)
		})
	}
}

// A Cancel after an unabsorbed failure keeps that failure's compensation,
// even with undo off.
func TestWorkflowCancelAfterFailure(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	require.NoError(t, e.store.Submit(ctx, wfPlan("cf", false, false,
		tk{name: "a", q: e.a, undo: e.c}, tk{name: "b", q: e.b}, tk{name: "c", q: e.c})))
	for _, q := range []*Queue{e.a, e.b.Queue} {
		p, ok := wfPop(t, q)
		require.True(t, ok)
		require.True(t, e.finish(p, failing("b")))
	}
	no := false
	require.NoError(t, e.store.Cancel(ctx, "cf", &no))
	assert.Equal(t, []string{"a:undo"}, e.drain(completeAll), "c stopped unrun, then a undone")
	r := e.record("cf")
	assert.Equal(t, contract.WorkflowCompensated, r.State)
	assert.Equal(t, contract.JobCancelled, r.Tasks[2].State)
}

// A FinishTask whose reply was lost, re-sent, changes nothing and names
// every push-target queue; a consumer that crashed before finishing has
// its item requeued and run elsewhere. Either way b is pushed and runs once.
func TestWorkflowLostFinish(t *testing.T) {
	end := contract.TaskEnd{Workflow: "cr", Task: "a", Outcome: contract.OutcomeCompleted}
	t.Run("reply lost", func(t *testing.T) {
		e := newWFTest(t)
		require.NoError(t, e.store.Submit(t.Context(), wfPlan("cr", false, true,
			tk{name: "a", q: e.a, undo: e.c}, tk{name: "b", q: e.b, deps: []string{"a"}})))
		a, ok := wfPop(t, e.a)
		require.True(t, ok)
		pushed, ok := e.store.FinishTask(e.a.QueueID(), a.ack, end)
		require.True(t, ok)
		assert.Equal(t, []string{e.b.queueKey}, pushed)
		pushed, ok = e.store.FinishTask(e.a.QueueID(), a.ack, end)
		require.True(t, ok)
		assert.Equal(t, []string{e.a.queueKey, e.c.queueKey, e.b.queueKey}, pushed, "every push-target queue")
		assert.Equal(t, []string{"b"}, e.drain(completeAll))
	})
	t.Run("crash before finish", func(t *testing.T) {
		e := newWFTest(t)
		require.NoError(t, e.store.Submit(t.Context(), wfPlan("cr", false, true,
			tk{name: "a", q: e.a}, tk{name: "b", q: e.b, deps: []string{"a"}})))
		crashed := e.qs.NewQueue("a")
		a, ok := wfPop(t, crashed)
		require.True(t, ok)
		require.NoError(t, crashed.Close(), "requeues a")
		assert.Equal(t, []string{"a", "b"}, e.drain(completeAll))
		assert.Equal(t, contract.WorkflowCompleted, e.record("cr").State)
		// The crashed consumer's late finish finds nothing to do.
		_, ok = e.store.FinishTask(e.a.QueueID(), a.ack, end)
		assert.True(t, ok)
		assert.Empty(t, e.drain(completeAll))
	})
}

// Purge cancels the workflow tasks it removes, queued or processing, and
// pushes what that releases after the delete, even into the purged queue.
func TestWorkflowPurgeCancels(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	require.NoError(t, e.store.Submit(ctx, wfPlan("p1", false, true,
		tk{name: "a", q: e.a}, tk{name: "b", q: e.b, deps: []string{"a"}})))
	require.NoError(t, e.store.Submit(ctx, wfPlan("p2", false, true,
		tk{name: "a", q: e.a}, tk{name: "x", q: e.c}, tk{name: "b", q: e.a, deps: []string{"a", "x"}, always: true})))
	require.True(t, e.a.Enqueue(`{"id":"wf:p1:zz","wf":"p1","task":"zz"}`), "a foreign task of p1: plain job")
	held, ok := wfPop(t, e.a) // p1's a, processing
	require.True(t, ok)
	require.Equal(t, "p1", held.wf)

	e.a.Purge()
	r := e.record("p1")
	assert.Equal(t, contract.WorkflowFailed, r.State, "a purge before Cancel counts as a failure")
	assert.Equal(t, map[string]contract.JobState{"a": contract.JobCancelled, "b": contract.JobCancelled}, taskStates(r))
	assert.Equal(t, "queue purged", r.Tasks[0].Reason)
	_, done := e.store.FinishTask(e.a.QueueID(), held.ack, contract.TaskEnd{Workflow: "p1", Task: "a", Outcome: contract.OutcomeCompleted})
	assert.True(t, done, "a finish of a purged item does nothing")

	r = e.record("p2")
	assert.Equal(t, contract.JobCancelled, r.Tasks[0].State)
	assert.Equal(t, contract.JobWaiting, r.Tasks[2].State, "b waits for x")
	assert.Equal(t, []string{"x", "b"}, e.drain(completeAll), "b pushed into a after the purge")
	assert.Equal(t, contract.WorkflowCompleted, e.record("p2").State, "a's cancel is absorbed by Always b")

	// A priority queue: its members carry the sequence prefix.
	require.NoError(t, e.store.Submit(ctx, wfPlan("p3", false, true,
		tk{name: "a", q: e.b, prio: 2}, tk{name: "b", q: e.c, deps: []string{"a"}, always: true})))
	e.b.Purge()
	r = e.record("p3")
	assert.Equal(t, contract.JobCancelled, r.Tasks[0].State)
	assert.Equal(t, []string{"b"}, e.drain(completeAll))
	assert.Equal(t, contract.WorkflowCompleted, e.record("p3").State)

	// A queue with no workflow item purges in one call.
	require.True(t, e.c.Enqueue("plain"))
	e.c.Purge()
	assert.Zero(t, e.c.client.LLen(ctx, e.c.queueKey).Val())
}

// Listing pages wf:index newest first, ties by ID, with a state filter.
func TestWorkflowListing(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	for _, id := range []string{"w1", "w2", "w3", "w4", "w5"} {
		require.NoError(t, e.store.Submit(ctx, wfPlan(id, false, true, tk{name: "a", q: e.a})))
	}
	// Fix the times: w5 newest, w2 and w3 tied.
	for id, at := range map[string]float64{"w1": 100, "w2": 200, "w3": 200, "w4": 300, "w5": 400} {
		require.NoError(t, e.a.client.ZAdd(ctx, e.store.indexKey(), redis.Z{Score: at, Member: id}).Err())
	}
	// w4 completes.
	for p, ok := wfPop(t, e.a); ok; p, ok = wfPop(t, e.a) {
		out := contract.OutcomeFailed
		if p.wf == "w4" {
			out = contract.OutcomeCompleted
		}
		_, done := e.store.FinishTask(e.a.QueueID(), p.ack, contract.TaskEnd{Workflow: p.wf, Task: p.task, Outcome: out})
		require.True(t, done)
	}
	ids := func(state contract.WorkflowState, offset, limit int) []string {
		keys, err := e.store.WorkflowKeys(ctx, state, offset, limit)
		require.NoError(t, err)
		var out []string
		for _, k := range keys {
			out = append(out, k.ID)
		}
		return out
	}
	assert.Equal(t, []string{"w5", "w4", "w2", "w3", "w1"}, ids("", 0, 10))
	assert.Equal(t, []string{"w4", "w2"}, ids("", 1, 2))
	assert.Equal(t, []string{"w3"}, ids("", 3, 1), "tie across the page edge")
	assert.Equal(t, []string{"w5", "w2", "w3", "w1"}, ids(contract.WorkflowFailed, 0, 10))
	assert.Equal(t, []string{"w3", "w1"}, ids(contract.WorkflowFailed, 2, 5))
	assert.Empty(t, ids(contract.WorkflowRunning, 0, 10))
	assert.Empty(t, ids("", 0, 0))
	assert.Equal(t, []string{"w2", "w3", "w1"}, ids("", 2, math.MaxInt), "no overflow")

	recs, err := e.store.Workflows(ctx, contract.WorkflowCompleted, 0, 10)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "w4", recs[0].ID)
	keys, err := e.store.WorkflowKeys(ctx, contract.WorkflowCompleted, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, []contract.WorkflowKey{{ID: "w4", CreatedAt: 300}}, keys)

	// A gone hash is skipped; the index trim drops old entries.
	require.NoError(t, e.a.client.Del(ctx, e.store.key("w5")).Err())
	assert.Equal(t, []string{"w4", "w2"}, ids("", 0, 2))
	e.store.trimIndex(ctx)
	assert.Equal(t, []string{"w4", "w3", "w2", "w1"}, e.a.client.ZRevRange(ctx, e.store.indexKey(), 0, -1).Val(),
		"all older than the retention; only w5, whose hash is gone, leaves")
}

// Submit refuses a plan with no task, a bad ID or name, a queue of another
// scope, or an item that is not an envelope.
func TestWorkflowSubmitRefuses(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	other := New(getTestRedisURL(), WithNamespace(newID()))
	defer other.Close()
	foreign := other.NewQueue("a")
	defer foreign.Close()

	assert.Error(t, e.store.Submit(ctx, contract.WorkflowPlan{ID: "empty"}))
	assert.Error(t, e.store.Submit(ctx, wfPlan("index", false, false, tk{name: "a", q: e.a})))
	assert.Error(t, e.store.Submit(ctx, wfPlan("ok", false, false, tk{name: "a:b", q: e.a})))
	assert.ErrorIs(t, e.store.Submit(ctx, wfPlan("ok", false, false, tk{name: "a", q: foreign})), contract.ErrScopeMismatch)
	assert.ErrorIs(t, e.store.Submit(ctx, wfPlan("ok", false, false, tk{name: "a", q: e.a, undo: foreign})), contract.ErrScopeMismatch)
	p := wfPlan("ok", false, false, tk{name: "a", q: e.a})
	p.Tasks[0].Item = []byte("not json")
	assert.Error(t, e.store.Submit(ctx, p))
	assert.Zero(t, e.a.client.Exists(ctx, e.store.key("ok")).Val())

	assert.Equal(t, e.store.ScopeID(), e.b.ScopeID())
	assert.NotEqual(t, e.store.ScopeID(), foreign.ScopeID())
	found, ok := contract.LookupWorkflowStore(e.a.ScopeID())
	assert.True(t, ok)
	assert.Same(t, e.store, found)
}

// A result over the limit fails the task; one that is not JSON is
// dropped; a task at the limit keeps its result.
func TestWorkflowResults(t *testing.T) {
	e := newWFTest(t, WithWorkflowResultLimit(8))
	require.NoError(t, e.store.Submit(t.Context(), wfPlan("res", false, true,
		tk{name: "big", q: e.a}, tk{name: "small", q: e.a}, tk{name: "bad", q: e.a},
		tk{name: "next", q: e.c, deps: []string{"big"}, always: true})))
	e.drain(func(p wfPopped) (contract.Outcome, []byte) {
		switch p.task {
		case "big":
			return contract.OutcomeCompleted, []byte(`"0123456789"`)
		case "bad":
			return contract.OutcomeCompleted, []byte(`{nope`)
		}
		return contract.OutcomeCompleted, []byte(`[[[12]]]`) // exactly the limit
	})
	r := e.record("res")
	assert.Equal(t, contract.WorkflowCompleted, r.State, "big's failure is absorbed by Always next")
	assert.Equal(t, contract.JobFailed, r.Tasks[0].State)
	assert.Equal(t, contract.ErrResultTooLarge.Error(), r.Tasks[0].Reason)
	assert.Equal(t, contract.JobCompleted, r.Tasks[1].State)
	assert.Equal(t, 8, r.Tasks[1].ResultSize)
	assert.Equal(t, contract.JobCompleted, r.Tasks[2].State)
	assert.Zero(t, r.Tasks[2].ResultSize)
	_, err := e.store.DepResult(t.Context(), "res", "big")
	assert.ErrorIs(t, err, contract.ErrDepFailed)
}

// A workflow that ended and expired: a late item is settled with no
// workflow step; the pre-run check reports it gone.
func TestWorkflowExpired(t *testing.T) {
	e := newWFTest(t, WithWorkflowRetention(50*time.Millisecond))
	ctx := t.Context()
	require.NoError(t, e.store.Submit(ctx, wfPlan("gone", false, true, tk{name: "a", q: e.a})))
	a, ok := wfPop(t, e.a)
	require.True(t, ok)
	require.True(t, e.finish(a, completeAll))
	require.Eventually(t, func() bool { return e.a.client.Exists(ctx, e.store.key("gone")).Val() == 0 }, 2*time.Second, 10*time.Millisecond)

	require.True(t, e.a.Enqueue(wfEnvelope("gone", "a", false, "1")), "a redelivered copy")
	a, ok = wfPop(t, e.a)
	require.True(t, ok)
	_, err := e.store.Runnable(ctx, "gone", "a", false)
	assert.ErrorIs(t, err, contract.ErrWorkflowNotFound)
	pushed, ok := e.store.FinishTask(e.a.QueueID(), a.ack, contract.TaskEnd{Workflow: "gone", Task: "a", Outcome: contract.OutcomeCancelled})
	assert.True(t, ok)
	assert.Empty(t, pushed)
	assert.Zero(t, e.a.GetNackedItemsCount(), "settled")
}

// A workflow item in a queue its workflow does not use, or naming a task
// it does not have, is settled with no workflow step.
func TestWorkflowForeignItem(t *testing.T) {
	e := newWFTest(t)
	require.NoError(t, e.store.Submit(t.Context(), wfPlan("fx", false, true,
		tk{name: "a", q: e.a}, tk{name: "b", q: e.a, deps: []string{"a"}})))
	require.True(t, e.c.Enqueue(wfEnvelope("fx", "a", false, "1")))
	p, ok := wfPop(t, e.c)
	require.True(t, ok)
	pushed, ok := e.store.FinishTask(e.c.QueueID(), p.ack, contract.TaskEnd{Workflow: "fx", Task: "a", Outcome: contract.OutcomeCompleted})
	assert.True(t, ok)
	assert.Empty(t, pushed)
	assert.Equal(t, contract.JobPending, e.record("fx").Tasks[0].State)

	require.True(t, e.a.Enqueue(`{"id":"x"}`))
	p, ok = wfPop(t, e.a) // the real a first
	require.True(t, ok)
	q, ok := wfPop(t, e.a)
	require.True(t, ok)
	_, ok = e.store.FinishTask(e.a.QueueID(), q.ack, contract.TaskEnd{Workflow: "bad id!", Task: "a", Outcome: contract.OutcomeCompleted})
	assert.True(t, ok, "invalid tag: settled as a plain job")
	assert.Equal(t, []string{"b"}, func() []string {
		require.True(t, e.finish(p, completeAll))
		return e.drain(completeAll)
	}())
}

// The cancel pop gate hands out a valid workflow item and keeps its mark;
// an item with crafted workflow fields is checked as a plain job.
func TestCancelGateWorkflowItem(t *testing.T) {
	e := newWFTest(t)
	q := e.a
	items := map[string]string{
		"wf:w:a":      string(wfEnvelope("w", "a", false, "1")),
		"wf:w:a:undo": string(wfEnvelope("w", "a", true, "1")),
		"wf:w:b":      `{"id":"wf:w:b","wf":"w"}`,                           // no task
		"wf:w:c":      `{"id":"wf:w:c","wf":"w","task":"c","undo":"yes"}`,   // undo not a bool
		"wf:x:d":      `{"id":"wf:x:d","wf":"w","task":"d"}`,                // id names another workflow
		"wf:w:e:undo": `{"id":"wf:w:e:undo","wf":"w","task":"e"}`,           // undo id without undo
		"wf:index:f":  `{"id":"wf:index:f","wf":"index","task":"f"}`,        // reserved ID
		"wf:w:g":      `{"id":"wf:w:g","wf":null,"task":"g"}`,               // null wf
		"wf:w:h":      `{"id":"wf:w:h","wf":"w","task":"h","WF":""}`,        // Go reads WF: a plain job there
		"wf:w:i":      `{"id":"wf:w:i","wf":"w","task":"i","tas\u212a":""}`, // Kelvin sign k: Go folds it
		"wf:w:j":      `{"id":"wf:w:j","wf":"w","task":"j","ID":"x"}`,       // Go reads ID
		"wf:w:k":      `{"id":"wf:w:k","wf":"w","task":"k","undo":null}`,    // null undo: Go reads false, valid
	}
	for id, v := range items {
		require.NoError(t, q.CancelJob(id))
		require.True(t, q.Enqueue(v))
	}
	var got []string
	for p, ok := wfPop(t, q); ok; p, ok = wfPop(t, q) {
		got = append(got, envID(t, p.raw))
	}
	assert.ElementsMatch(t, []string{"wf:w:a", "wf:w:a:undo", "wf:w:k"}, got)
	assert.True(t, marked(t, q, "wf:w:a"), "a handed-out workflow item keeps its mark")
	assert.False(t, marked(t, q, "wf:w:b"))
}

// Two varmq worker processes, each serving part of the queues, and a
// producer process run one workflow end to end (examples/workflow).
func TestWorkflowTwoProcesses(t *testing.T) {
	if _, err := exec.LookPath("go"); testing.Short() || err != nil {
		t.Skip("builds examples/workflow with the go tool")
	}
	bin := filepath.Join(t.TempDir(), "workflow")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = filepath.Join("examples", "workflow")
	out, err := build.CombinedOutput()
	if err != nil && strings.Contains(string(out), "requires go >=") {
		t.Skipf("examples/workflow needs a newer go: %s", out) // CI on go.mod's go with GOTOOLCHAIN=local
	}
	require.NoError(t, err, string(out))

	env := append(os.Environ(), "REDIS_URL="+getTestRedisURL(), "REDIS_NS="+newID())
	var logs [2]bytes.Buffer
	var workers []*exec.Cmd
	stop := func() {
		for _, w := range workers {
			_ = w.Process.Signal(syscall.SIGTERM)
			_ = w.Wait()
		}
		workers = nil
	}
	defer stop()
	for i, queues := range []string{"upload,thumb", "email,unsend"} {
		w := exec.Command(bin, "worker", queues)
		w.Env, w.Stdout, w.Stderr = env, &logs[i], &logs[i]
		require.NoError(t, w.Start())
		workers = append(workers, w)
	}

	submit := exec.Command(bin, "submit")
	submit.Env = env
	res, err := submit.Output()
	require.NoError(t, err, string(res))
	var rec contract.WorkflowRecord
	require.NoError(t, json.Unmarshal(res, &rec), string(res))
	assert.Equal(t, contract.WorkflowCompleted, rec.State)
	require.Len(t, rec.Tasks, 4)
	for _, tr := range rec.Tasks {
		assert.Equal(t, contract.JobCompleted, tr.State, tr.Name)
	}
	assert.Equal(t, contract.JobWaiting, rec.Tasks[3].Undo.State, "completed: the undo never ran")
	assert.Positive(t, rec.Tasks[1].ResultSize)

	stop()
	assert.Contains(t, logs[0].String(), "upload photo.png")
	assert.Contains(t, logs[0].String(), "thumb 512")
	assert.Contains(t, logs[1].String(), "email to ann@example.com with s3://bucket/photo.png@64px", "results cross processes")
	assert.NotContains(t, logs[1].String(), "undo")
}

// Guards: bad options, bad IDs, unknown outcomes, closed queues, queues
// without a live store object, and errors from a closed client.
func TestWorkflowGuards(t *testing.T) {
	qs0 := New(getTestRedisURL(), WithNamespace(newID()))
	defer qs0.Close()
	_, err0 := NewWorkflowStore(qs0, WithWorkflowResultLimit(0))
	assert.Error(t, err0)
	_, err0 = NewWorkflowStore(qs0, WithWorkflowRetention(0))
	assert.Error(t, err0)

	e := newWFTest(t)
	ctx := t.Context()
	for _, id := range []string{"", "index", "a:b"} {
		assert.ErrorIs(t, e.store.Cancel(ctx, id, nil), contract.ErrWorkflowNotFound)
		_, err := e.store.Record(ctx, id)
		assert.ErrorIs(t, err, contract.ErrWorkflowNotFound)
		_, err = e.store.Runnable(ctx, id, "a", false)
		assert.ErrorIs(t, err, contract.ErrWorkflowNotFound)
	}
	yes := true
	require.NoError(t, e.store.Submit(ctx, wfPlan("g", false, false, tk{name: "a", q: e.a, undo: e.c})))
	require.NoError(t, e.store.Cancel(ctx, "g", &yes))
	assert.True(t, e.record("g").UndoOnCancel, "Cancel's undo overrides the plan's")
	run, err := e.store.Runnable(ctx, "g", "bad name!", false)
	assert.NoError(t, err)
	assert.False(t, run)

	a, ok := wfPop(t, e.a)
	require.True(t, ok)
	_, ok = e.store.FinishTask(e.a.QueueID(), a.ack, contract.TaskEnd{Workflow: "g", Task: "a", Outcome: "odd"})
	assert.False(t, ok, "unknown outcome")

	// A queue key outside the store's scope or layout is refused.
	for _, id := range []string{"redisq:other:fifo:{a}", e.a.queueKey + "}", "redisq:fifo:{a}", e.store.prefix + "fifo:a", e.store.prefix + "xxxx:{a}", e.store.prefix + "fifo:{a{b}"} {
		_, ok = e.store.FinishTask(id, a.ack, contract.TaskEnd{Workflow: "g", Task: "a", Outcome: contract.OutcomeCompleted})
		assert.False(t, ok, id)
	}
	// A queue made by newQueueBase alone has no scope: plain purge.
	bare := newQueueBase(e.a.client, e.a.queueKey)
	assert.Empty(t, bare.ScopeID())
	_, ok = e.store.FinishTask(e.a.QueueID(), a.ack, contract.TaskEnd{Workflow: "g", Task: "a", Outcome: contract.OutcomeCompleted})
	assert.True(t, ok)
	bare.Purge()
	assert.Zero(t, e.a.client.Exists(ctx, e.a.getStatsKey()).Val())

	// Closed queue and closed client.
	closed := e.qs.NewQueue("z")
	require.NoError(t, closed.Close())
	_, ok = e.store.FinishTask(closed.QueueID(), "x", contract.TaskEnd{Workflow: "g", Task: "a", Outcome: contract.OutcomeCompleted})
	assert.True(t, ok, "a closed queue's ack is stale: settled through the key, nothing to do")
	qs := New(getTestRedisURL(), WithNamespace(newID()))
	q := qs.NewQueue("a")
	defer q.Close()
	store, err := NewWorkflowStore(qs)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, qs.Close())
	assert.Error(t, store.Submit(ctx, wfPlan("g", false, false, tk{name: "a", q: q})))
	assert.Error(t, store.Cancel(ctx, "g", nil))
	_, err = store.Record(ctx, "g")
	assert.Error(t, err)
	_, err = store.Runnable(ctx, "g", "a", false)
	assert.Error(t, err)
	_, err = store.Workflows(ctx, "", 0, 10)
	assert.Error(t, err)
	_, ok = store.FinishTask(q.QueueID(), "x", contract.TaskEnd{Workflow: "g", Task: "a", Outcome: contract.OutcomeCompleted})
	assert.False(t, ok)
	q.Purge()                             // logs the error
	store.trimIndex(context.Background()) // logs the error

	// A meta that is not JSON: reads fail, listings skip it.
	require.NoError(t, e.a.client.HSet(ctx, e.store.key("g"), "meta", "{nope").Err())
	_, err = e.store.Record(ctx, "g")
	assert.Error(t, err)
	_, err = e.store.Runnable(ctx, "g", "a", false)
	assert.Error(t, err)
	_, err = e.store.DepResult(ctx, "g", "a")
	assert.Error(t, err)
	recs, err := e.store.Workflows(ctx, "", 0, 10)
	require.NoError(t, err)
	assert.Empty(t, recs)

	// A workflow key of the wrong type: listing and purge report the error.
	require.NoError(t, e.a.client.Set(ctx, e.store.key("g"), "x", 0).Err())
	_, err = e.store.WorkflowKeys(ctx, "", 0, 10)
	assert.Error(t, err)
	require.True(t, e.a.Enqueue(`{"id":"wf:g:a","wf":"g","task":"a"}`))
	assert.Error(t, e.a.purge())
}

// Purge of a queue holding items of more workflows than Lua can unpack at
// once; listing through more same-millisecond entries than one chunk; a
// too-deep item.
func TestWorkflowScale(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()

	pipe := e.a.client.Pipeline()
	for i := range 9000 {
		pipe.RPush(ctx, e.a.queueKey, wfEnvelope(fmt.Sprintf("gone%d", i), "a", false, "1"))
	}
	_, err := pipe.Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, e.a.purge())
	assert.Zero(t, e.a.client.LLen(ctx, e.a.queueKey).Val())

	pipe = e.a.client.Pipeline()
	var want []string
	for i := range 450 {
		id := fmt.Sprintf("t%03d", i)
		want = append(want, id)
		pipe.HSet(ctx, e.store.key(id), "meta", `{"state":"running","created_at":5,"tasks":[]}`)
		pipe.ZAdd(ctx, e.store.indexKey(), redis.Z{Score: 5, Member: id})
	}
	_, err = pipe.Exec(ctx)
	require.NoError(t, err)
	keys, err := e.store.WorkflowKeys(ctx, "", 0, 1000)
	require.NoError(t, err)
	var got []string
	for _, k := range keys {
		got = append(got, k.ID)
	}
	assert.Equal(t, want, got, "every tie once, by ID")
	keys, err = e.store.WorkflowKeys(ctx, contract.WorkflowRunning, 440, 5)
	require.NoError(t, err)
	assert.Len(t, keys, 5)
	// A filter reads at most 10000 + limit entries: t000..t449 (oldest)
	// sit past that many newer ones.
	pipe = e.a.client.Pipeline()
	for i := range 10100 {
		id := fmt.Sprintf("n%05d", i)
		pipe.HSet(ctx, e.store.key(id), "meta", `{"state":"completed","created_at":9,"tasks":[]}`)
		pipe.ZAdd(ctx, e.store.indexKey(), redis.Z{Score: 9, Member: id})
	}
	_, err = pipe.Exec(ctx)
	require.NoError(t, err)
	keys, err = e.store.WorkflowKeys(ctx, contract.WorkflowRunning, 0, 5)
	require.NoError(t, err)
	assert.Empty(t, keys, "running ones lie past the scan cap")

	// Items Redis's cjson can't decode are refused at Submit: as a task, as
	// a waiting task, as an undo; nothing is written.
	deep := strings.Repeat("[", 1100) + strings.Repeat("]", 1100)
	for i, p := range []contract.WorkflowPlan{
		wfPlan("bad1", false, false, tk{name: "a", q: e.a, data: deep}),
		wfPlan("bad2", false, false, tk{name: "a", q: e.a}, tk{name: "b", q: e.a, deps: []string{"a"}, data: `"\ud800"`}),
		wfPlan("bad3", false, false, tk{name: "a", q: e.a, undo: e.c}),
	} {
		if i == 2 {
			p.Tasks[0].Undo.Item = wfEnvelope("bad3", "a", true, `"\udc00x"`)
		}
		assert.ErrorContains(t, e.store.Submit(ctx, p), "cjson", p.ID)
		assert.Zero(t, e.a.client.Exists(ctx, e.store.key(p.ID)).Val(), p.ID)
	}
	assert.Zero(t, e.a.client.LLen(ctx, e.a.queueKey).Val())
}

// Every pop path, FIFO and priority, hands out a marked valid workflow item
// and drops a marked plain one.
func TestCancelGateWorkflowItemEveryPop(t *testing.T) {
	pops := map[string]func(q *Queue) []string{
		"raw": func(q *Queue) []string {
			var got []string
			for v, ok := rawPopOf(q); ok; v, ok = rawPopOf(q) {
				got = append(got, string(v.([]byte)))
			}
			return got
		},
		"single": func(q *Queue) []string {
			var got []string
			for v, ok, _ := ackPopOf(q); ok; v, ok, _ = ackPopOf(q) {
				got = append(got, string(v.([]byte)))
			}
			return got
		},
		"lean batch": func(q *Queue) []string {
			var got []string
			for {
				q.popLen.Store(1)
				items, ids := make([]any, 4), make([]string, 4)
				n, _, err := batchPopOf(q, items, ids)
				if err != nil || n == 0 {
					return got
				}
				for _, v := range items[:n] {
					got = append(got, string(v.([]byte)))
				}
			}
		},
		"batch": func(q *Queue) []string {
			items, ids := make([]any, 4), make([]string, 4)
			n, _, _ := batchPopOf(q, items, ids)
			var got []string
			for _, v := range items[:n] {
				got = append(got, string(v.([]byte)))
			}
			return got
		},
	}
	for name, pop := range pops {
		t.Run(name, func(t *testing.T) {
			eachCancelCase(t, func(t *testing.T, c cancelQueues) {
				task := string(wfEnvelope("w", "a", false, "1"))
				require.NoError(t, c.prod.CancelJob("wf:w:a"))
				require.NoError(t, c.prod.CancelJob("plain"))
				require.True(t, c.push(envelope("plain", `1`), 1))
				require.True(t, c.push(task, 1))
				assert.Equal(t, []string{task}, pop(c.cons))
				assert.True(t, marked(t, c.cons, "wf:w:a"))
				assert.False(t, marked(t, c.cons, "plain"))
			})
		})
	}
}

// Consumers on four objects finish a wide workflow at once: each task runs
// once, the join after all of them once; concurrent Submits of one plan
// push its root once. Also: 261 tasks (more than one wfload chunk), no TTL
// while running.
func TestWorkflowConcurrent(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	tasks := []tk{{name: "root", q: e.a}}
	var mid []string
	for i := range 260 {
		name := fmt.Sprintf("m%03d", i)
		mid = append(mid, name)
		tasks = append(tasks, tk{name: name, q: e.a, deps: []string{"root"}})
	}
	tasks = append(tasks, tk{name: "join", q: e.c, deps: mid})
	p := wfPlan("wide", false, false, tasks...)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { assert.NoError(t, e.store.Submit(ctx, p)) })
	}
	wg.Wait()
	assert.EqualValues(t, 1, e.a.client.LLen(ctx, e.a.queueKey).Val(), "root pushed once")
	assert.EqualValues(t, -1, e.a.client.PTTL(ctx, e.store.key("wide")).Val(), "running: no TTL")

	var mu sync.Mutex
	runs := map[string]int{}
	for range 4 {
		wg.Go(func() {
			qs := New(getTestRedisURL(), WithNamespace(strings.TrimPrefix(strings.TrimSuffix(e.store.prefix, ":"), "redisq:")))
			defer qs.Close()
			store := e.store // other clients' queues: it builds them from the key
			mine := []*Queue{qs.NewQueue("a"), qs.NewQueue("c")}
			defer mine[0].Close()
			defer mine[1].Close()
			idle := 0
			for idle < 20 {
				got := false
				for _, q := range mine {
					if p, ok := wfPop(t, q); ok {
						got = true
						end := contract.TaskEnd{Workflow: p.wf, Task: p.task, Outcome: contract.OutcomeCompleted, Result: []byte(`1`)}
						if run, err := store.Runnable(ctx, p.wf, p.task, false); err == nil && run {
							mu.Lock()
							runs[p.task]++
							mu.Unlock()
						} else {
							end.Outcome = contract.OutcomeCancelled
						}
						_, ok := store.FinishTask(q.QueueID(), p.ack, end)
						assert.True(t, ok)
					}
				}
				if got {
					idle = 0
				} else {
					idle++
					time.Sleep(10 * time.Millisecond)
				}
			}
		})
	}
	wg.Wait()
	r := e.record("wide")
	assert.Equal(t, contract.WorkflowCompleted, r.State)
	assert.Len(t, runs, 262)
	for name, n := range runs {
		assert.Equal(t, 1, n, name)
	}
	assert.Equal(t, contract.JobCompleted, r.Tasks[261].State)
}

// FinishTask applies a workflow step only for the settled item's own task;
// a script that misses a queue in KEYS changes nothing and asks to reread.
func TestWorkflowFinishChecks(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	require.NoError(t, e.store.Submit(ctx, wfPlan("fc", false, false,
		tk{name: "a", q: e.a}, tk{name: "b", q: e.a}, tk{name: "c", q: e.c, deps: []string{"a"}})))
	a, ok := wfPop(t, e.a)
	require.True(t, ok)
	require.Equal(t, "a", a.task)
	pushed, ok := e.store.FinishTask(e.a.QueueID(), a.ack, contract.TaskEnd{Workflow: "fc", Task: "b", Outcome: contract.OutcomeFailed})
	assert.True(t, ok)
	assert.Empty(t, pushed)
	assert.Equal(t, map[string]contract.JobState{"a": contract.JobPending, "b": contract.JobPending, "c": contract.JobWaiting},
		taskStates(e.record("fc")), "a settled alone; b untouched")

	r, err := wfCancelScript.Run(ctx, e.a.client, []string{e.store.key("fc")}, "", "", int64(1000)).Int64Slice()
	require.NoError(t, err)
	assert.Equal(t, []int64{2}, r, "no queues in KEYS: reread")
	assert.Equal(t, contract.WorkflowRunning, e.record("fc").State)
	calls := 0
	assert.ErrorIs(t, reread(func() error { calls++; return errReread }), errReread)
	assert.Equal(t, wfTries, calls)

	// An unreadable workflow (another writer's bad t:) settles items alone.
	require.NoError(t, e.a.client.HSet(ctx, e.store.key("fc"), "t:b", "{nope").Err())
	b, ok := wfPop(t, e.a)
	require.True(t, ok)
	_, ok = e.store.FinishTask(e.a.QueueID(), b.ack, contract.TaskEnd{Workflow: "fc", Task: "b", Outcome: contract.OutcomeCompleted})
	assert.True(t, ok)
	assert.Zero(t, e.a.GetNackedItemsCount())
	assert.ErrorContains(t, e.store.Cancel(ctx, "fc", nil), "unreadable")
}

// Review round 3: a long chain fails without overflowing Lua's call depth;
// a re-sent applying Purge changes nothing; Submit refuses a queue with an
// expiration or a key that isn't UTF-8; trimIndex runs once a minute.
func TestWorkflowRobust(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()

	const n = 21000
	tasks := []tk{{name: "c0", q: e.a}}
	for i := 1; i < n; i++ {
		tasks = append(tasks, tk{name: fmt.Sprintf("c%d", i), q: e.a, deps: []string{fmt.Sprintf("c%d", i-1)}})
	}
	require.NoError(t, e.store.Submit(ctx, wfPlan("chain", false, false, tasks...)))
	assert.Equal(t, []string{"c0"}, e.drain(failing("c0")))
	r := e.record("chain")
	assert.Equal(t, contract.WorkflowFailed, r.State)
	assert.Equal(t, contract.JobCancelled, r.Tasks[n-1].State)

	// The applying run sets its once-key; a re-send with it returns at once.
	require.NoError(t, e.store.Submit(ctx, wfPlan("rs", false, false,
		tk{name: "a", q: e.a}, tk{name: "b", q: e.a, deps: []string{"a"}, always: true})))
	once := e.a.queueKey + ":purge:test"
	require.NoError(t, e.a.client.Set(ctx, once, "1", time.Minute).Err())
	keys := append(e.a.requeueKeys(), e.a.getJobsKey(), e.a.getCompletedKey(), e.a.getFailedKey(), e.a.getStatsKey(), e.a.getCancelledKey(), once)
	got, err := wfPurgeScript.Run(ctx, e.a.client, keys, len(keys)-1, 0, "", "fifo", 1000).Slice()
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1)}, got)
	assert.EqualValues(t, 1, e.a.client.LLen(ctx, e.a.queueKey).Val(), "re-send: nothing deleted")
	e.a.Purge()
	assert.Equal(t, map[string]contract.JobState{"a": contract.JobCancelled, "b": contract.JobPending}, taskStates(e.record("rs")))
	assert.EqualValues(t, 1, e.a.client.LLen(ctx, e.a.queueKey).Val(), "b pushed after the delete")

	exp := e.qs.NewQueue("exp")
	defer exp.Close()
	exp.SetExpiration(time.Hour)
	assert.ErrorContains(t, e.store.Submit(ctx, wfPlan("x1", false, false, tk{name: "a", q: exp})), "expiration")
	bad := e.qs.NewQueue("caf\xe9")
	defer bad.Close()
	assert.ErrorContains(t, e.store.Submit(ctx, wfPlan("x2", false, false, tk{name: "a", q: bad})), "UTF-8")

	e.store.trimmedAt.Store(time.Now().UnixMilli())
	require.NoError(t, e.a.client.ZAdd(ctx, e.store.indexKey(), redis.Z{Score: 1, Member: "old"}).Err())
	e.store.trimIndex(ctx)
	assert.EqualValues(t, 1, e.a.client.ZScore(ctx, e.store.indexKey(), "old").Val(), "trimmed under a minute ago: skipped")
	e.store.trimmedAt.Store(0)
	e.store.trimIndex(ctx)
	assert.Equal(t, redis.Nil, e.a.client.ZScore(ctx, e.store.indexKey(), "old").Err())
}

// NewWorkflowStore registers the scope's store with contract; a second one
// for the scope is refused until the first closes.
func TestNewWorkflowStoreRegistry(t *testing.T) {
	ns := newID()
	qs := New(getTestRedisURL(), WithNamespace(ns))
	defer qs.Close()
	qs2 := New(getTestRedisURL(), WithNamespace(ns))
	defer qs2.Close()
	other := New(getTestRedisURL(), WithNamespace(newID()))
	defer other.Close()
	q := qs.NewQueue("a")
	defer q.Close()

	// The scope needs no store, and is the same across clients and kinds.
	scope := q.ScopeID()
	assert.NotEmpty(t, scope)
	assert.Equal(t, scope, qs2.NewPriorityQueue("b").ScopeID())
	assert.NotEqual(t, scope, other.NewQueue("a").ScopeID())
	_, found := contract.LookupWorkflowStore(scope)
	assert.False(t, found)

	s, err := NewWorkflowStore(qs, WithWorkflowResultLimit(10), WithWorkflowRetention(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, scope, s.ScopeID())
	assert.Equal(t, 10, s.resultLimit)
	assert.Equal(t, time.Hour, s.retention)
	got, found := contract.LookupWorkflowStore(scope)
	require.True(t, found)
	assert.Same(t, s, got)
	assert.Contains(t, contract.WorkflowStores(), contract.WorkflowStore(s))

	_, err = NewWorkflowStore(qs2)
	assert.ErrorIs(t, err, contract.ErrScopeRegistered, "one store per scope")
	s2, err := NewWorkflowStore(other)
	require.NoError(t, err, "another scope")
	assert.NoError(t, s2.Close())

	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "idempotent")
	_, found = contract.LookupWorkflowStore(scope)
	assert.False(t, found)
	s3, err := NewWorkflowStore(qs2)
	require.NoError(t, err, "the scope is free after Close")
	assert.NoError(t, s3.Close())

	_, err = NewWorkflowStore(qs, WithWorkflowResultLimit(0))
	assert.Error(t, err)
	_, err = NewWorkflowStore(qs, WithWorkflowRetention(0))
	assert.Error(t, err)
	_, err = NewWorkflowStore(nil)
	assert.Error(t, err)
	_, found = contract.LookupWorkflowStore(scope)
	assert.False(t, found, "a failed constructor registers nothing")
}

// A queue with no store in its process still runs plain jobs, and a purge
// still cancels the workflow tasks it removes, with the default retention.
func TestQueueWithoutWorkflowStore(t *testing.T) {
	qs := New(getTestRedisURL(), WithNamespace(newID()))
	defer qs.Close()
	q := qs.NewQueue("plain")
	defer q.Close()
	pq := qs.NewPriorityQueue("plain-prio")
	defer pq.Close()

	require.True(t, q.Enqueue(`{"id":"j1","data":1}`))
	_, ok, ack := q.DequeueWithAckId()
	require.True(t, ok)
	assert.True(t, q.Acknowledge(ack))
	require.True(t, pq.Enqueue(`{"id":"j2","data":2}`, 1))
	_, ok, ack = pq.DequeueWithAckId()
	require.True(t, ok)
	assert.True(t, pq.Acknowledge(ack))

	s, err := NewWorkflowStore(qs)
	require.NoError(t, err)
	require.NoError(t, s.Submit(t.Context(), wfPlan("np", false, false, tk{name: "a", q: q})))
	require.NoError(t, s.Close())
	q.Purge()
	rec, err := s.Record(t.Context(), "np")
	require.NoError(t, err)
	assert.Equal(t, contract.JobCancelled, rec.Tasks[0].State, "a purged task ends")
}

// A store of one client finishes tasks of a queue that another client of the
// scope made, and of a priority queue, by the key alone.
func TestWorkflowStoreFinishesByQueueKey(t *testing.T) {
	e := newWFTest(t)
	other := New(getTestRedisURL(), WithNamespace(strings.TrimPrefix(strings.TrimSuffix(e.store.prefix, ":"), "redisq:")))
	defer other.Close()
	oa, ob := other.NewQueue("a"), other.NewPriorityQueue("b")
	defer oa.Close()
	defer ob.Close()

	require.NoError(t, e.store.Submit(t.Context(), wfPlan("key", false, false,
		tk{name: "a", q: e.a}, tk{name: "b", q: e.b, deps: []string{"a"}})))
	a, ok := wfPop(t, oa)
	require.True(t, ok)
	pushed, ok := e.store.FinishTask(oa.QueueID(), a.ack, contract.TaskEnd{Workflow: "key", Task: "a", Outcome: contract.OutcomeCompleted, Result: []byte(`1`)})
	require.True(t, ok)
	assert.Equal(t, []string{e.b.queueKey}, pushed)
	b, ok := wfPop(t, ob.Queue)
	require.True(t, ok)
	_, ok = e.store.FinishTask(ob.QueueID(), b.ack, contract.TaskEnd{Workflow: "key", Task: "b", Outcome: contract.OutcomeCompleted})
	require.True(t, ok)
	assert.Equal(t, contract.WorkflowCompleted, e.record("key").State)

	// Each queue object's own record retention applies, whichever client made it.
	oc := other.NewQueue("d")
	defer oc.Close()
	require.NoError(t, e.store.Submit(t.Context(), wfPlan("ret", false, false,
		tk{name: "a", q: e.a}, tk{name: "b", q: oc})))
	e.a.SetCompletedRetention(0, 0)
	oc.SetCompletedRetention(0, 0)
	ra, ok := wfPop(t, e.a)
	require.True(t, ok)
	rc, ok := wfPop(t, oc)
	require.True(t, ok)
	end := func(task string) contract.TaskEnd {
		return contract.TaskEnd{Workflow: "ret", Task: task, Outcome: contract.OutcomeCompleted}
	}
	_, ok = e.store.FinishTask(e.a.QueueID(), ra.ack, end("a"))
	require.True(t, ok)
	_, ok = e.store.FinishTask(oc.QueueID(), rc.ack, end("b"))
	require.True(t, ok)
	ctx := t.Context()
	assert.Zero(t, e.a.client.ZCard(ctx, e.a.getCompletedKey()).Val(), "a keeps no record")
	assert.Zero(t, e.a.client.ZCard(ctx, oc.getCompletedKey()).Val(), "the other client's queue keeps none")
}

// WorkflowCounts is one number per state, all seven present, kept by every
// state write (submit, finish, cancel, purge); a lost or drifted states ZSET
// heals from the hashes on the next call.
func TestWorkflowCounts(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	counts := func() map[contract.WorkflowState]int64 {
		t.Helper()
		m, err := e.store.WorkflowCounts(ctx)
		require.NoError(t, err)
		return m
	}
	want := func(running, completed, failed, compensating, compensated, compFailed, cancelled int64) map[contract.WorkflowState]int64 {
		return map[contract.WorkflowState]int64{contract.WorkflowRunning: running, contract.WorkflowCompleted: completed,
			contract.WorkflowFailed: failed, contract.WorkflowCompensating: compensating, contract.WorkflowCompensated: compensated,
			contract.WorkflowCompensationFailed: compFailed, contract.WorkflowCancelled: cancelled}
	}
	assert.Equal(t, want(0, 0, 0, 0, 0, 0, 0), counts(), "empty store: every state, zero")

	submit := func(id string, tasks ...tk) {
		t.Helper()
		require.NoError(t, e.store.Submit(ctx, wfPlan(id, false, false, tasks...)))
	}
	submit("done", tk{name: "ok", q: e.a})
	submit("fail", tk{name: "bad", q: e.b})
	submit("saga", tk{name: "s1", q: e.a, undo: e.c}, tk{name: "s2", q: e.b, deps: []string{"s1"}})
	submit("stuck", tk{name: "u1", q: e.a, undo: e.c}, tk{name: "u2", q: e.c, deps: []string{"u1"}})
	submit("cx", tk{name: "z", q: e.c})
	assert.Equal(t, want(5, 0, 0, 0, 0, 0, 0), counts())
	require.NoError(t, e.store.Cancel(ctx, "cx", nil))
	e.drain(failing("bad", "s2", "u2", "u1:undo"))
	assert.Equal(t, want(0, 1, 1, 0, 1, 1, 1), counts())

	// One compensating (its undo still queued), two running.
	submit("comp", tk{name: "k1", q: e.a, undo: e.c}, tk{name: "k2", q: e.b, deps: []string{"k1"}})
	for _, q := range []*Queue{e.a, e.b.Queue} {
		p, ok := wfPop(t, q)
		require.True(t, ok)
		require.True(t, e.finish(p, failing("k2")))
	}
	submit("run", tk{name: "r", q: e.a})
	submit("run2", tk{name: "r", q: e.b})
	all := want(2, 1, 1, 1, 1, 1, 1)
	assert.Equal(t, all, counts())
	c, states := e.a.client, e.store.statesKey()
	assert.Equal(t, int64(8), c.ZCard(ctx, states).Val())
	assert.Equal(t, float64(4), c.ZScore(ctx, states, "comp").Val())

	// Backfill: data of a version before the states ZSET.
	require.NoError(t, c.Del(ctx, states).Err())
	assert.Equal(t, all, counts(), "rebuilt from the hashes")
	assert.Equal(t, int64(8), c.ZCard(ctx, states).Val())
	// An older instance trimmed the index only: the extra state leaves.
	require.NoError(t, c.ZAdd(ctx, states, redis.Z{Score: 1, Member: "ghost"}).Err())
	assert.Equal(t, all, counts())
	assert.Equal(t, redis.Nil, c.ZScore(ctx, states, "ghost").Err())
	assert.Equal(t, int64(8), c.ZCard(ctx, states).Val())
	// An index entry whose hash is gone, added by an older instance: score
	// 0, counted nowhere, sizes agree (no heal on each call).
	require.NoError(t, c.ZAdd(ctx, e.store.indexKey(), redis.Z{Score: 1, Member: "lost"}).Err())
	assert.Equal(t, all, counts())
	assert.Equal(t, []redis.Z{{Score: 0, Member: "lost"}}, c.ZRangeWithScores(ctx, states, 0, 0).Val())

	// The trim drops the entry from both.
	e.store.trimmedAt.Store(0)
	e.store.trimIndex(ctx)
	assert.Equal(t, redis.Nil, c.ZScore(ctx, states, "lost").Err())
	assert.Equal(t, int64(8), c.ZCard(ctx, states).Val())

	// Purge ends run's task: running -> failed.
	e.a.Purge()
	assert.Equal(t, want(1, 1, 2, 1, 1, 1, 1), counts())
}

// A finished workflow whose hash expired counts (as it lists in wf:index)
// until the trim drops it.
func TestWorkflowCountsTrim(t *testing.T) {
	e := newWFTest(t, WithWorkflowRetention(50*time.Millisecond))
	ctx := t.Context()
	require.NoError(t, e.store.Submit(ctx, wfPlan("old", false, false, tk{name: "a", q: e.a})))
	e.drain(completeAll)
	require.Eventually(t, func() bool { return e.a.client.Exists(ctx, e.store.key("old")).Val() == 0 }, 2*time.Second, 10*time.Millisecond)
	m, err := e.store.WorkflowCounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), m[contract.WorkflowCompleted], "stale until the trim")

	e.store.trimmedAt.Store(0)
	e.store.trimIndex(ctx)
	m, err = e.store.WorkflowCounts(ctx)
	require.NoError(t, err)
	assert.Zero(t, m[contract.WorkflowCompleted])
	assert.Zero(t, e.a.client.Exists(ctx, e.store.statesKey()).Val())
}

// A task record carries when it was pushed, when the pre-run check let it
// run and when it ended, on the Redis clock; so does its undo. As varmq's
// TestMemStoreTaskTimes.
func TestWorkflowTaskTimes(t *testing.T) {
	e := newWFTest(t)
	ctx := t.Context()
	c := e.a.client
	// tick is the Redis clock, after a pause: stamps of two steps differ.
	tick := func() int64 {
		time.Sleep(2 * time.Millisecond)
		return c.Time(ctx).Val().UnixMilli()
	}
	times := func(r contract.TaskRecord) contract.TaskRecord {
		return contract.TaskRecord{EnqueuedAt: r.EnqueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
	}
	task := func(name string) contract.TaskRecord {
		t.Helper()
		for _, tr := range e.record("w").Tasks {
			if tr.Name == name {
				return tr
			}
		}
		t.Fatalf("no task %s", name)
		return contract.TaskRecord{}
	}
	in := func(lo, v, hi int64, msg string) {
		t.Helper()
		assert.True(t, lo <= v && v <= hi, "%s: %d not in [%d, %d]", msg, v, lo, hi)
	}
	end := func(p wfPopped, out contract.Outcome) {
		t.Helper()
		_, ok := e.store.FinishTask(p.q.QueueID(), p.ack, contract.TaskEnd{Workflow: p.wf, Task: p.task, Undo: p.undo, Outcome: out})
		require.True(t, ok)
	}
	runnable := func(name string, undo bool) bool {
		t.Helper()
		ok, err := e.store.Runnable(ctx, "w", name, undo)
		require.NoError(t, err)
		return ok
	}

	m0 := tick()
	require.NoError(t, e.store.Submit(ctx, wfPlan("w", false, false,
		tk{name: "a", q: e.a, undo: e.c},
		tk{name: "b", q: e.b, deps: []string{"a"}},
		tk{name: "c", q: e.a, deps: []string{"b"}})))
	m1 := tick()
	a := task("a")
	t0 := a.EnqueuedAt
	in(m0, t0, m1, "root pushed at submit")
	assert.Equal(t, contract.TaskRecord{EnqueuedAt: t0}, times(a))
	assert.Equal(t, contract.TaskRecord{}, times(task("b")), "waiting: no times")
	assert.Equal(t, contract.TaskRecord{}, times(*a.Undo), "undo not pushed: no times")

	require.True(t, runnable("a", false))
	require.False(t, runnable("b", false))
	m2 := tick()
	t1 := task("a").StartedAt
	in(m1, t1, m2, "a started")
	assert.Zero(t, task("b").StartedAt, "a refused check stamps nothing")

	pa, ok := wfPop(t, e.a)
	require.True(t, ok)
	end(pa, contract.OutcomeCompleted)
	m3 := tick()
	t2 := task("a").FinishedAt
	in(m2, t2, m3, "a finished")
	assert.Equal(t, contract.TaskRecord{EnqueuedAt: t0, StartedAt: t1, FinishedAt: t2}, times(task("a")))
	assert.Equal(t, contract.TaskRecord{EnqueuedAt: t2}, times(task("b")), "released by a's end")

	pb, ok := wfPop(t, e.b.Queue)
	require.True(t, ok)
	end(pb, contract.OutcomeFailed)
	m4 := tick()
	t3 := task("b").FinishedAt
	in(m3, t3, m4, "b finished")
	assert.Equal(t, contract.TaskRecord{EnqueuedAt: t2, FinishedAt: t3}, times(task("b")))
	assert.Equal(t, contract.TaskRecord{FinishedAt: t3}, times(task("c")), "cancelled while waiting")
	assert.Equal(t, contract.TaskRecord{EnqueuedAt: t3}, times(*task("a").Undo), "undo pushed")

	require.False(t, runnable("a", false), "ended: no restamp")
	require.True(t, runnable("a", true))
	m5 := tick()
	t4 := task("a").Undo.StartedAt
	in(m4, t4, m5, "undo started")
	pu, ok := wfPop(t, e.c)
	require.True(t, ok)
	require.True(t, pu.undo)
	end(pu, contract.OutcomeCompleted)
	t5 := task("a").Undo.FinishedAt
	in(m5, t5, tick(), "undo finished")
	a = task("a")
	assert.Equal(t, contract.TaskRecord{EnqueuedAt: t3, StartedAt: t4, FinishedAt: t5}, times(*a.Undo))
	assert.Equal(t, contract.TaskRecord{EnqueuedAt: t0, StartedAt: t1, FinishedAt: t2}, times(a), "the undo leaves the task's own times")
	assert.Equal(t, contract.JobCompleted, a.Undo.State)
	assert.Equal(t, contract.WorkflowCompensated, e.record("w").State)

	// The listing reads the same fields; a time not reached has no field.
	recs, err := e.store.Workflows(ctx, "", 0, 10)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, e.record("w").Tasks, recs[0].Tasks)
	h := c.HGetAll(ctx, e.store.key("w")).Val()
	assert.Equal(t, strconv.FormatInt(t1, 10), h["sa:a"])
	for _, f := range []string{"sa:b", "ea:c", "sa:c"} {
		assert.NotContains(t, h, f)
	}
}

// Runnable never makes a hash (unknown or expired ID) and leaves the TTL of
// a finished workflow as it is.
func TestWorkflowRunnableNoWrite(t *testing.T) {
	e := newWFTest(t, WithWorkflowRetention(time.Hour))
	ctx := t.Context()
	c := e.a.client
	for _, undo := range []bool{false, true} {
		_, err := e.store.Runnable(ctx, "nope", "a", undo)
		assert.ErrorIs(t, err, contract.ErrWorkflowNotFound)
	}
	assert.Zero(t, c.Exists(ctx, e.store.key("nope")).Val(), "no ghost hash")

	require.NoError(t, e.store.Submit(ctx, wfPlan("fin", false, false, tk{name: "a", q: e.a, undo: e.c})))
	ok, err := e.store.Runnable(ctx, "fin", "a", false)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, time.Duration(-1), c.PTTL(ctx, e.store.key("fin")).Val(), "running: the stamp sets no TTL")
	e.drain(completeAll)
	before := e.record("fin")
	for _, undo := range []bool{false, true} {
		ok, err := e.store.Runnable(ctx, "fin", "a", undo)
		require.NoError(t, err)
		assert.False(t, ok)
	}
	ttl := c.PTTL(ctx, e.store.key("fin")).Val()
	assert.True(t, ttl > 59*time.Minute && ttl <= time.Hour, "TTL kept: %v", ttl)
	assert.Equal(t, before, e.record("fin"), "no stamp after the end")

	// A compensating workflow has no TTL either; the undo stamp sets none.
	require.NoError(t, e.store.Submit(ctx, wfPlan("und", false, false,
		tk{name: "a", q: e.a, undo: e.c}, tk{name: "b", q: e.b, deps: []string{"a"}})))
	for _, q := range []*Queue{e.a, e.b.Queue} {
		p, ok := wfPop(t, q)
		require.True(t, ok)
		require.True(t, e.finish(p, failing("b")))
	}
	ok, err = e.store.Runnable(ctx, "und", "a", true)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, time.Duration(-1), c.PTTL(ctx, e.store.key("und")).Val())
	assert.Equal(t, []string{"a:undo"}, e.drain(completeAll), "the undo still runs after the stamp")
	assert.Equal(t, contract.WorkflowCompensated, e.record("und").State)
}
