package redisq

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPriorityQueueEnqueueDequeue(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Test items with different priorities (1.0 is highest priority)
	items := []struct {
		value    string
		priority int
	}{
		{"high", 1},
		{"medium", 2},
		{"low", 3},
	}

	// Enqueue items
	for _, item := range items {
		assert.True(t, pq.Enqueue(item.value, item.priority))
	}

	// Verify queue length
	assert.Equal(t, len(items), pq.Len())

	// Dequeue items and verify order (1.0 priority first)
	expected := []string{"high", "medium", "low"}
	for _, exp := range expected {
		item, ok := pq.Dequeue()
		assert.True(t, ok)
		assert.Equal(t, exp, string(item.([]byte)))
	}

	// Verify queue is empty
	assert.Equal(t, 0, pq.Len())
}

func TestPriorityQueueValues(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	items := []struct {
		value    string
		priority int
	}{
		{"high", 1},
		{"medium", 2},
		{"low", 3},
	}

	for _, item := range items {
		assert.True(t, pq.Enqueue(item.value, item.priority))
	}

	values := pq.Values()
	assert.Len(t, values, len(items))

	// Verify order (1.0 priority first)
	expected := []string{"high", "medium", "low"}
	for i, exp := range expected {
		assert.Equal(t, exp, string(values[i].([]byte)))
	}
}

func TestPriorityQueueRemove(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Test items with different priorities
	items := []struct {
		value    string
		priority int
	}{
		{"item1", 1},
		{"item2", 2},
		{"item3", 3},
	}

	// Enqueue items
	for _, item := range items {
		assert.True(t, pq.Enqueue(item.value, item.priority))
	}

	// Verify initial queue length
	assert.Equal(t, len(items), pq.Len())

	// Get values to find the serialized version of item2
	item2Bytes := []byte(items[1].value)

	require.NotNil(t, item2Bytes, "Failed to find item2 in queue values")

	// Remove item2 from the queue
	assert.True(t, pq.Remove(item2Bytes))

	// Verify queue length decreased by 1
	assert.Equal(t, len(items)-1, pq.Len())

	// Verify remaining items
	var foundItem2 bool
	for _, val := range pq.Values() {
		if string(val.([]byte)) == "item2" {
			foundItem2 = true
			break
		}
	}
	assert.False(t, foundItem2, "item2 should have been removed from the queue")

	// Attempt to remove an item that doesn't exist
	assert.True(t, pq.Remove("nonexistent-item"), "Remove should return true even for non-existent items")

	// Verify queue length hasn't changed
	assert.Equal(t, len(items)-1, pq.Len())
}

func setupTestPriorityQueue(t *testing.T) (*PriorityQueue, func()) {
	redisURL := getTestRedisURL()
	qs := New(redisURL)
	pq := qs.NewPriorityQueue(testQueueKey)

	// Clear the queue before test
	ctx := context.Background()
	_, err := pq.client.Ping(ctx).Result()
	if err != nil {
		t.Skip("Redis server is not available, skipping test: " + err.Error())
		return nil, func() {}
	}

	require.NoError(t, pq.client.Del(ctx, testQueueKey).Err(), "Failed to clear test queue")
	require.NoError(t, pq.client.Del(ctx, pq.getNackedItemKey()).Err(), "Failed to clear nacked queue")
	require.NoError(t, pq.client.Del(ctx, pq.getTimeoutKey()).Err(), "Failed to clear timeout key")

	cleanup := func() {
		pq.client.Del(ctx, testQueueKey)
		pq.client.Del(ctx, pq.getNackedItemKey())
		pq.client.Del(ctx, pq.getTimeoutKey())
		pq.Close()
		qs.Close()
	}

	return pq, cleanup
}

func TestPriorityQueueDequeueWithAckId(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Test with empty queue
	item, ok, ackID := pq.DequeueWithAckId()
	assert.Nil(t, item, "Item should be nil when queue is empty")
	assert.False(t, ok, "ok should be false when queue is empty")
	assert.Equal(t, "", ackID, "ackID should be empty when queue is empty")

	// Add items to the queue
	testItem := "test-priority-item"
	assert.True(t, pq.Enqueue(testItem, 1), "Enqueue should succeed")

	// Dequeue with acknowledgment
	dequeuedItem, ok, ackID := pq.DequeueWithAckId()
	assert.True(t, ok, "Dequeue should succeed")

	// Check if the item is a byte slice
	if byteItem, isByteSlice := dequeuedItem.([]byte); isByteSlice {
		assert.Equal(t, testItem, string(byteItem), "Dequeued item should match")
	} else {
		assert.Equal(t, testItem, dequeuedItem, "Dequeued item should match")
	}

	assert.NotEmpty(t, ackID, "Should generate a non-empty ackID")

	// Verify queue is now empty
	assert.Equal(t, 0, pq.Len(), "Queue should be empty after dequeue")

	// Verify item is in the nacked items list
	count := pq.GetNackedItemsCount()
	assert.Equal(t, 1, count, "Should have 1 item in nacked list")

	// Acknowledge the item
	result := pq.Acknowledge(ackID)
	assert.True(t, result, "Acknowledge should succeed")

	// Verify item is removed from nacked list
	count = pq.GetNackedItemsCount()
	assert.Equal(t, 0, count, "Should have 0 items in nacked list after acknowledgment")
}

func TestPriorityQueueRequeueNackedItems(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Set short visibility timeout for testing
	pq.SetVisibilityTimeout(100 * time.Millisecond)

	// Add items to the queue
	pq.Enqueue("high-priority", 1)
	pq.Enqueue("low-priority", 3)

	// Dequeue them with ack IDs
	item1, ok1, ackID1 := pq.DequeueWithAckId()
	assert.True(t, ok1, "First dequeue should succeed")
	item2, ok2, ackID2 := pq.DequeueWithAckId()
	assert.True(t, ok2, "Second dequeue should succeed")

	_ = item1
	_ = item2
	_ = ackID1
	_ = ackID2

	// Verify queue is empty
	assert.Equal(t, 0, pq.Len(), "Queue should be empty")

	// Verify items are in nacked list
	assert.Equal(t, 2, pq.GetNackedItemsCount(), "Should have 2 items in nacked list")

	// Set ack timeout for expiration
	pq.SetAckTimeout(1 * time.Second)

	// Wait for visibility timeout to expire
	time.Sleep(200 * time.Millisecond)

	// Requeue nacked items
	err := pq.RequeueNackedItems()
	assert.NoError(t, err, "RequeueNackedItems should succeed")

	// Verify items are back in queue
	assert.Equal(t, 2, pq.Len(), "Queue should have 2 items after requeue")
	assert.Equal(t, 0, pq.GetNackedItemsCount(), "Nacked list should be empty")
}

func TestPriorityQueueEnqueueClosed(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Close the queue
	pq.Close()

	// Attempt to enqueue
	ok := pq.Enqueue("test", 1)
	assert.False(t, ok, "Enqueue should fail when queue is closed")
}

func TestPriorityQueueDequeueClosed(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Add an item first
	pq.Enqueue("test", 1)

	// Close the queue
	pq.Close()

	// Attempt to dequeue
	_, ok := pq.Dequeue()
	assert.False(t, ok, "Dequeue should fail when queue is closed")
}

func TestPriorityQueueEnqueueInvalidType(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Attempt to enqueue an unsupported type
	ok := pq.Enqueue(12345, 1) // int is not supported
	assert.False(t, ok, "Enqueue should fail with unsupported type")

	// Verify queue is still empty
	assert.Equal(t, 0, pq.Len(), "Queue should be empty after failed enqueue")
}

func TestPriorityQueueExpiration(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Set short expiration
	expiration := time.Second
	pq.SetExpiration(expiration)

	// Enqueue item
	assert.True(t, pq.Enqueue("test", 1), "Enqueue should succeed")

	// Verify item exists
	assert.Equal(t, 1, pq.Len(), "Queue should have 1 item")

	// Wait for expiration
	time.Sleep(expiration + 100*time.Millisecond)

	// Verify queue is empty after expiration
	assert.Equal(t, 0, pq.Len(), "Queue should be empty after expiration")
}

func TestPriorityQueueRequeueNackedItemsClosed(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Close the queue
	pq.Close()

	// Attempt to requeue nacked items
	err := pq.RequeueNackedItems()
	assert.Error(t, err, "RequeueNackedItems should fail when queue is closed")
	assert.Contains(t, err.Error(), "queue closed", "Error should mention queue closed")
}

func TestPriorityQueueRequeueWithoutVisibilityTimeout(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Disable visibility timeout to use the old behavior path
	pq.SetVisibilityTimeout(0)

	// Add items to the queue
	pq.Enqueue("item1", 1)
	pq.Enqueue("item2", 2)

	// Manually add items to nacked list (simulating failed acks)
	ctx := context.Background()
	pq.client.HSet(ctx, pq.getNackedItemKey(), "test-ack-1", "nacked-item-1")
	pq.client.HSet(ctx, pq.getNackedItemKey(), "test-ack-2", "nacked-item-2")

	// Verify nacked items are there
	assert.Equal(t, 2, pq.GetNackedItemsCount(), "Should have 2 items in nacked list")

	// Requeue nacked items (without visibility timeout, should get all items)
	err := pq.RequeueNackedItems()
	assert.NoError(t, err, "RequeueNackedItems should succeed")

	// Verify nacked list is now empty
	assert.Equal(t, 0, pq.GetNackedItemsCount(), "Nacked list should be empty after requeue")

	// Verify items were requeued (queue should have original 2 + requeued 2 = 4)
	assert.Equal(t, 4, pq.Len(), "Queue should have 4 items after requeue")
}

func TestPriorityQueueDequeueWithAckIdClosed(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Add item
	pq.Enqueue("test", 1)

	// Close the queue
	pq.Close()

	// DequeueWithAckId should fail
	item, ok, ackID := pq.DequeueWithAckId()
	assert.Nil(t, item, "Item should be nil when queue is closed")
	assert.False(t, ok, "ok should be false when queue is closed")
	assert.Empty(t, ackID, "ackID should be empty when queue is closed")
}

func TestPriorityQueueDequeueWithAckIdAndTimeout(t *testing.T) {
	pq, cleanup := setupTestPriorityQueue(t)
	defer cleanup()

	// Set both visibility timeout and ack timeout
	pq.SetVisibilityTimeout(1 * time.Second)
	pq.SetAckTimeout(5 * time.Second)

	// Enqueue item
	pq.Enqueue("test-item", 1)

	// Dequeue with ack ID
	item, ok, ackID := pq.DequeueWithAckId()
	assert.True(t, ok, "Dequeue should succeed")
	assert.NotEmpty(t, ackID, "ackID should not be empty")

	if byteItem, isByteSlice := item.([]byte); isByteSlice {
		assert.Equal(t, "test-item", string(byteItem))
	}

	// Verify item is in the timeout ZSET
	ctx := context.Background()
	timeoutKey := pq.queueKey + ":timeouts"
	zCount, err := pq.client.ZCard(ctx, timeoutKey).Result()
	assert.NoError(t, err, "ZCard should succeed")
	assert.Equal(t, int64(1), zCount, "Should have 1 item in timeout ZSET")

	// Acknowledge and verify cleanup
	pq.Acknowledge(ackID)
	zCount, _ = pq.client.ZCard(ctx, timeoutKey).Result()
	assert.Equal(t, int64(0), zCount, "Timeout ZSET should be empty after ack")
}
