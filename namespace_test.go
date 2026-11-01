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

	assert.Equal(t, "varmq:ns_test:fifo:{orders}", q.queueKey)
	assert.Equal(t, "varmq:ns_test:prio:{orders}", pq.queueKey)
	assert.Equal(t, "varmq:ns_test:fifo:{orders}:idle", q.getIdleKey())

	ctx := context.Background()
	q.client.Del(ctx, q.queueKey, pq.queueKey, pq.queueKey+":seq")

	// A FIFO and a priority queue of one name are separate queues.
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

	plain := New(getTestRedisURL())
	defer plain.Close()
	assert.Panics(t, func() { plain.NewQueue("x{y}") })
	q := plain.NewQueue("orders")
	defer q.Close()
	assert.Equal(t, "varmq:fifo:{orders}", q.queueKey)
}
