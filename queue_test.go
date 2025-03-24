package redisq

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testQueueKey = "test_queue"

func getTestRedisURL() string {
	if err := godotenv.Load(); err != nil {
		panic(err)
	}

	if url := os.Getenv("REDIS_URL"); url != "" {
		return url
	}

	panic("REDIS_URL environment variable is not set")
}

func setupTestQueue(t *testing.T) (*RedisQueue, func()) {
	redisURL := getTestRedisURL()
	q := NewRedisQueue(testQueueKey, redisURL)

	// Clear the queue before test
	ctx := context.Background()
	require.NoError(t, q.client.Del(ctx, testQueueKey).Err(), "Failed to clear test queue")

	cleanup := func() {
		q.client.Del(ctx, testQueueKey)
		q.Close()
	}

	return q, cleanup
}

func TestNewRedisQueue(t *testing.T) {
	redisURL := getTestRedisURL()
	q := NewRedisQueue(testQueueKey, redisURL)
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
}
