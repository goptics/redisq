package redisq

import (
	"context"
	"fmt"
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
	{"list", listPushWakeOnce, func(qs Queues, key string) (*Queue, func(string, string) bool, func() int, func()) {
		q := qs.NewQueue(key)
		return q, func(v, id string) bool { return q.EnqueueOnce(v, id) }, q.Len, func() { q.Close() }
	}},
	{"priority", zsetPushWakeOnce, func(qs Queues, key string) (*Queue, func(string, string) bool, func() int, func()) {
		q := qs.NewPriorityQueue(key)
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

			q.SetDedupWindow(500 * time.Microsecond) // under 1ms: kept for 1ms, not an error
			assert.True(t, push("a", "w"))
			assert.Equal(t, 3, size())

			ctx := context.Background()
			pttl := func(id string) time.Duration { return q.client.PTTL(ctx, q.queueKey+":dedup:"+id).Val() }
			q.SetDedupWindow(time.Minute)
			assert.True(t, push("a", "k"))
			before := pttl("k")
			require.Positive(t, before)
			q.SetDedupWindow(0)
			assert.True(t, push("a", "k"), "no window: a remembered id pushes")
			assert.Equal(t, 5, size())
			assert.LessOrEqual(t, pttl("k"), before, "nor refreshes the id")

			assert.True(t, push("a", "z"))
			assert.True(t, push("a", "z"))
			assert.Equal(t, 7, size(), "no window: no dedupe")
			q.SetDedupWindow(-time.Second)
			assert.True(t, push("a", "n"))
			assert.True(t, push("a", "n"))
			assert.Equal(t, 9, size(), "a negative window: no dedupe")
			assert.Zero(t, q.client.Exists(ctx, q.queueKey+":dedup:z", q.queueKey+":dedup:n").Val(), "no id key")
		})
	}
}

// lastArg records the last argument of each EVALSHA of script hash.
type lastArg struct {
	hash string
	mx   sync.Mutex
	args []any
}

func (h *lastArg) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *lastArg) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *lastArg) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if a := cmd.Args(); len(a) > 1 && a[0] == "evalsha" && a[1] == h.hash {
			h.mx.Lock()
			h.args = append(h.args, a[len(a)-1])
			h.mx.Unlock()
		}
		return next(ctx, cmd)
	}
}

// EnqueueOnce sends a window under 1ms as 1ms, and none as 0.
func TestEnqueueOnceWindowArg(t *testing.T) {
	for _, c := range dedupCases {
		t.Run(c.name, func(t *testing.T) {
			qs := New(getTestRedisURL())
			defer qs.Close()
			client := qs.(*queues).client
			delKeys(t, client, c.key())
			defer delKeys(t, client, c.key())
			require.NoError(t, c.script.Load(context.Background(), client).Err()) // so every push is an EVALSHA
			h := &lastArg{hash: c.script.Hash()}
			client.AddHook(h)
			q, push, _, done := c.open(qs, c.key())
			defer done()

			for i, d := range []time.Duration{500 * time.Microsecond, 0, -time.Second, 2 * time.Second} {
				q.SetDedupWindow(d)
				require.True(t, push("a", fmt.Sprint("w", i)))
			}
			h.mx.Lock()
			defer h.mx.Unlock()
			assert.Equal(t, []any{int64(1), int64(0), int64(0), int64(2000)}, h.args)
		})
	}
}

// A push that fails after its id was checked (a key of the wrong type) must
// leave the id free: a retry once the key is fixed stores the item.
func TestEnqueueOnceFailedPushKeepsIdFree(t *testing.T) {
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

			bad := q.queueKey
			if strings.Contains(c.name, "priority") {
				bad += ":seq" // INCR fails on it
			}
			require.NoError(t, client.Set(ctx, bad, "x", 0).Err())
			assert.False(t, push("a", "id-1"), "the push fails")
			require.NoError(t, client.Del(ctx, bad).Err())

			assert.True(t, push("a", "id-1"))
			assert.Equal(t, 1, size(), "the retry stores the item")
		})
	}
}

// EnqueueOnce keeps the priority it is given.
func TestEnqueueOnceKeepsPriority(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	key := "test_dedup_keeps_priority"
	delKeys(t, qs.(*queues).client, key)
	defer delKeys(t, qs.(*queues).client, key)
	pq := qs.NewPriorityQueue(key)
	defer pq.Close()

	for i, p := range []int{3, 1, 2} {
		require.True(t, pq.EnqueueOnce(fmt.Sprint(p), p, fmt.Sprint("id-", i)))
	}
	var got []string
	for range 3 {
		v, ok := pq.Dequeue()
		require.True(t, ok)
		got = append(got, string(v.([]byte)))
	}
	assert.Equal(t, []string{"1", "2", "3"}, got)
}

// EnqueueOnce wakes an idle consumer; a repeat publishes nothing and leaves
// it idle.
func TestEnqueueOnceWakesOnlyOnAPush(t *testing.T) {
	for _, c := range dedupCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			qs := New(getTestRedisURL())
			defer qs.Close()
			client := qs.(*queues).client
			delKeys(t, client, c.key())
			defer delKeys(t, client, c.key())
			q, push, _, done := c.open(qs, c.key())
			defer done()
			n := q.Notification

			n.Subscribe(func(string) {}) // joins the idle set
			// the consumer's own channel: only a PUBLISH reaches it, not the
			// nudge loop's in-process broadcast
			wake := client.Subscribe(ctx, n.wakePrefix+n.id)
			defer wake.Close()
			_, err := wake.Receive(ctx)
			require.NoError(t, err)

			require.True(t, push("a", "x"))
			msg, err := wake.ReceiveTimeout(ctx, 2*time.Second)
			require.NoError(t, err, "the push woke no one")
			assert.Equal(t, "enqueued", msg.(*redis.Message).Payload)

			_, ok := rawPopOf(q)
			require.True(t, ok)
			assert.Zero(t, n.len()) // back in the idle set
			before := publishes(t, client)
			require.True(t, push("a", "x"))
			assert.Equal(t, before, publishes(t, client), "a repeat publishes nothing")
			assert.True(t, client.SIsMember(ctx, n.idleKey, n.id).Val(), "the consumer stays idle")
		})
	}
}

// EnqueueOnce sets the queue's expiration like Enqueue does.
func TestEnqueueOnceSetsExpiration(t *testing.T) {
	for _, c := range dedupCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			qs := New(getTestRedisURL())
			defer qs.Close()
			client := qs.(*queues).client
			delKeys(t, client, c.key())
			defer delKeys(t, client, c.key())
			q, push, _, done := c.open(qs, c.key())
			defer done()

			q.SetExpiration(10 * time.Second)
			require.True(t, push("a", "x"))
			ttl := client.PTTL(ctx, q.queueKey).Val()
			assert.Greater(t, ttl, time.Duration(0))
			assert.LessOrEqual(t, ttl, 10*time.Second)
		})
	}
}
