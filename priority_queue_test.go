package redisq

import (
	"context"
	"testing"

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

func setupTestPriorityQueue(t *testing.T) (*PriorityQueue, func()) {
	redisURL := getTestRedisURL()
	qs := New(redisURL)
	pq := qs.NewPriorityQueue(testQueueKey)

	// Clear the queue before test
	ctx := context.Background()
	require.NoError(t, pq.client.Del(ctx, testQueueKey).Err(), "Failed to clear test queue")

	cleanup := func() {
		pq.client.Del(ctx, testQueueKey)
		pq.Close()
		qs.Close()
	}

	return pq, cleanup
}
