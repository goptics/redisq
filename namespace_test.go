package redisq

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNamespace(t *testing.T) {
	qs := New(getTestRedisURL(), WithNamespace("ns_test"))
	defer qs.Close()

	q := qs.NewQueue("orders")
	defer q.Close()
	pq := qs.NewPriorityQueue("orders")
	defer pq.Close()

	assert.Equal(t, "ns_test:fifo:{orders}", q.queueKey)
	assert.Equal(t, "ns_test:prio:{orders}", pq.queueKey)
	assert.Equal(t, "ns_test:fifo:{orders}:idle", q.getIdleKey())

	ctx := context.Background()
	q.client.Del(ctx, q.queueKey, pq.queueKey, pq.queueKey+":seq")

	// A FIFO and a priority queue of one name are separate queues; on the
	// legacy layout they would share one key and fail with WRONGTYPE.
	require.True(t, q.Enqueue("a"))
	require.True(t, pq.Enqueue("b", 1))
	item, ok := q.Dequeue()
	require.True(t, ok)
	assert.Equal(t, []byte("a"), item)
	item, ok = pq.Dequeue()
	require.True(t, ok)
	assert.Equal(t, []byte("b"), item)
}

func TestNamespaceRejectsBraces(t *testing.T) {
	assert.Panics(t, func() { New(getTestRedisURL(), WithNamespace("a{b}")) })

	qs := New(getTestRedisURL(), WithNamespace("ns_test"))
	defer qs.Close()
	assert.Panics(t, func() { qs.NewQueue("x{y}") })
	assert.Panics(t, func() { qs.NewQueue("") })

	legacy := New(getTestRedisURL(), WithNamespace(""))
	defer legacy.Close()
	q := legacy.NewQueue("legacy_{key}") // legacy layout keeps any key as is
	defer q.Close()
	assert.Equal(t, "legacy_{key}", q.queueKey)
}
