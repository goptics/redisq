package redisq

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The method sets varmq's IDedupQueue and IDedupPriorityQueue look for.
var (
	_ interface{ EnqueueOnce(any, string) bool }      = (*Queue)(nil)
	_ interface{ EnqueueOnce(any, string) bool }      = (*DistributedQueue)(nil)
	_ interface{ EnqueueOnce(any, int, string) bool } = (*PriorityQueue)(nil)
	_ interface{ EnqueueOnce(any, int, string) bool } = (*DistributedPriorityQueue)(nil)
)

// dedupCase is a queue kind and the script its EnqueueOnce runs.
type dedupCase struct {
	name   string
	script *redis.Script
	open   func(qs Queues, key string) (q *Queue, push func(item, id string) bool, size func() int, done func())
}

var dedupCases = []dedupCase{
	{"list", listPushOnce, func(qs Queues, key string) (*Queue, func(string, string) bool, func() int, func()) {
		q := qs.NewQueue(key)
		return q, func(v, id string) bool { return q.EnqueueOnce(v, id) }, q.Len, func() { q.Close() }
	}},
	{"priority", zsetPushOnce, func(qs Queues, key string) (*Queue, func(string, string) bool, func() int, func()) {
		q := qs.NewPriorityQueue(key)
		return q.Queue, func(v, id string) bool { return q.EnqueueOnce(v, 1, id) }, q.Len, func() { q.Close() }
	}},
	{"distributed", listPushWakeOnce, func(qs Queues, key string) (*Queue, func(string, string) bool, func() int, func()) {
		q := qs.NewDistributedQueue(key)
		return q.Queue, func(v, id string) bool { return q.EnqueueOnce(v, id) }, q.Len, func() { q.Close() }
	}},
	{"distributed priority", zsetPushWakeOnce, func(qs Queues, key string) (*Queue, func(string, string) bool, func() int, func()) {
		q := qs.NewDistributedPriorityQueue(key)
		return q.Queue, func(v, id string) bool { return q.EnqueueOnce(v, 1, id) }, q.Len, func() { q.Close() }
	}},
}

func (c dedupCase) key() string { return "test_dedup_" + strings.ReplaceAll(c.name, " ", "_") }

// A push whose reply is lost is re-sent by go-redis. With an id, the re-send
// must not store the item again.
func TestEnqueueOnceReSentAfterLostReplyStoresOnce(t *testing.T) {
	for _, c := range dedupCases {
		t.Run(c.name, func(t *testing.T) {
			p := newDropProxy(t, c.script)
			var late atomic.Bool
			p.onDrop = func() { // set before any traffic: proxy goroutines read it
				if late.Load() {
					time.Sleep(1200 * time.Millisecond) // a re-send long after the run
				}
			}
			qs := New(p.url())
			defer qs.Close()
			client := qs.(*queues).client
			delKeys(t, client, c.key())
			defer delKeys(t, client, c.key())
			require.NoError(t, c.script.Load(context.Background(), client).Err()) // so every push is an EVALSHA the proxy sees
			_, push, size, done := c.open(qs, c.key())
			defer done()

			p.drops.Store(1)
			assert.True(t, push("a", "id-a"))
			assert.EqualValues(t, 2, p.sends.Load(), "the push should run, lose its reply and be re-sent once")
			assert.Equal(t, 1, size(), "the re-send must not push again")

			p.drops.Store(4) // go-redis's 1 try + 3 retries all lose their reply
			assert.False(t, push("b", "id-b"))
			assert.EqualValues(t, 6, p.sends.Load())
			assert.Equal(t, 2, size(), "four runs of one call store it once")

			late.Store(true)
			p.drops.Store(1)
			assert.True(t, push("c", "id-c"))
			assert.Equal(t, 3, size(), "a late re-send must still find the id")
		})
	}
}

// An id is stored once per window, by any queue object, however many calls
// race on it; a job without an id is a plain push.
func TestEnqueueOnceStoresAnIdOnce(t *testing.T) {
	for _, c := range dedupCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			qs := New(getTestRedisURL())
			defer qs.Close()
			client := qs.(*queues).client
			delKeys(t, client, c.key())
			defer delKeys(t, client, c.key())
			q, push, size, done := c.open(qs, c.key())
			defer done()
			_, push2, _, done2 := c.open(qs, c.key()) // another producer
			defer done2()

			assert.True(t, push("a", "x"))
			assert.True(t, push("a", "x"), "a repeat is accepted")
			assert.True(t, push2("a", "x"), "another producer's repeat too")
			assert.Equal(t, 1, size())

			var wg sync.WaitGroup
			for range 64 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					assert.True(t, push("b", "y"))
				}()
			}
			wg.Wait()
			assert.Equal(t, 2, size(), "racing calls on one id store it once")

			assert.True(t, push("c", ""))
			assert.True(t, push("c", ""))
			assert.Equal(t, 4, size(), "no id: a plain push each time")

			ttl := client.PTTL(ctx, q.queueKey+":dedup:x").Val()
			assert.Greater(t, ttl, 5*time.Minute-10*time.Second, "the id is kept for the window")
			assert.LessOrEqual(t, ttl, 5*time.Minute)
		})
	}
}

// After the window an id is new again; a window of 0 turns dedupe off.
func TestEnqueueOnceWindow(t *testing.T) {
	for _, c := range dedupCases {
		t.Run(c.name, func(t *testing.T) {
			qs := New(getTestRedisURL())
			defer qs.Close()
			delKeys(t, qs.(*queues).client, c.key())
			defer delKeys(t, qs.(*queues).client, c.key())
			q, push, size, done := c.open(qs, c.key())
			defer done()

			q.SetDedupWindow(200 * time.Millisecond)
			assert.True(t, push("a", "x"))
			assert.True(t, push("a", "x"))
			assert.Equal(t, 1, size())
			time.Sleep(300 * time.Millisecond)
			assert.True(t, push("a", "x"))
			assert.Equal(t, 2, size(), "the window passed")

			q.SetDedupWindow(0)
			assert.True(t, push("a", "z"))
			assert.True(t, push("a", "z"))
			assert.Equal(t, 4, size(), "no window: no dedupe")
		})
	}
}
