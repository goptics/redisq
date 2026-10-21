package redisq

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushCase is a queue kind and the script its Enqueue runs.
type pushCase struct {
	name   string
	script *redis.Script
	open   func(qs Queues, key string) (push func(string) bool, size func() int, done func())
}

var pushCases = []pushCase{
	{"list", listPush, func(qs Queues, key string) (func(string) bool, func() int, func()) {
		q := qs.NewQueue(key)
		return func(s string) bool { return q.Enqueue(s) }, q.Len, func() { q.Close() }
	}},
	{"priority", zsetPush, func(qs Queues, key string) (func(string) bool, func() int, func()) {
		q := qs.NewPriorityQueue(key)
		return func(s string) bool { return q.Enqueue(s, 1) }, q.Len, func() { q.Close() }
	}},
	{"distributed", listPushWake, func(qs Queues, key string) (func(string) bool, func() int, func()) {
		q := qs.NewDistributedQueue(key)
		return func(s string) bool { return q.Enqueue(s) }, q.Len, func() { q.Close() }
	}},
	{"distributed priority", zsetPushWake, func(qs Queues, key string) (func(string) bool, func() int, func()) {
		q := qs.NewDistributedPriorityQueue(key)
		return func(s string) bool { return q.Enqueue(s, 1) }, q.Len, func() { q.Close() }
	}},
}

func (c pushCase) key() string { return "test_push_" + strings.ReplaceAll(c.name, " ", "_") }

// A push whose reply is lost is re-sent by go-redis. The re-send must not
// store the item again: that ran the job twice.
func TestPushReSentAfterLostReplyStoresOnce(t *testing.T) {
	for _, c := range pushCases {
		t.Run(c.name, func(t *testing.T) {
			p := newDropProxy(t, c.script)
			qs := New(p.url())
			defer qs.Close()
			client := qs.(*queues).client
			delKeys(t, client, c.key())
			defer delKeys(t, client, c.key())
			require.NoError(t, c.script.Load(context.Background(), client).Err()) // so every push is an EVALSHA the proxy sees
			push, size, done := c.open(qs, c.key())
			defer done()

			p.drops.Store(1)
			assert.True(t, push("a"))
			assert.EqualValues(t, 2, p.sends.Load(), "the push should run, lose its reply and be re-sent once")
			assert.Equal(t, 1, size(), "the re-send must not push again")

			p.drops.Store(4) // go-redis's 1 try + 3 retries all lose their reply
			assert.False(t, push("b"))
			assert.EqualValues(t, 6, p.sends.Load())
			assert.Equal(t, 2, size(), "four runs of one call store it once")

			assert.True(t, push("b"), "a new call is a new push")
			assert.Equal(t, 3, size())
		})
	}
}

// Every call gets its own token: concurrent pushes of one item from one
// queue object all land, and the tokens stay small integer sets that expire.
func TestPushTokensKeepEveryCall(t *testing.T) {
	const workers, each = 16, 100
	for _, c := range pushCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			qs := New(getTestRedisURL())
			defer qs.Close()
			client := qs.(*queues).client
			delKeys(t, client, c.key())
			defer delKeys(t, client, c.key())
			push, size, done := c.open(qs, c.key())
			defer done()

			var wg sync.WaitGroup
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range each {
						assert.True(t, push("same"))
					}
				}()
			}
			wg.Wait()
			assert.Equal(t, workers*each, size())

			buckets, err := client.Keys(ctx, c.key()+":push:*").Result()
			require.NoError(t, err)
			assert.Len(t, buckets, workers*each/pushBucket+1)
			for _, b := range buckets {
				assert.Equal(t, "intset", client.ObjectEncoding(ctx, b).Val(), "bucket %s", b)
				ttl := client.PTTL(ctx, b).Val()
				assert.Greater(t, ttl, time.Duration(0), "bucket %s must expire", b)
				assert.LessOrEqual(t, ttl, time.Minute)
			}
		})
	}
}

// A token must outlive every re-send of its call: a read timeout, a retry
// backoff, a new connection and a write, per retry.
func TestPushTTLCoversRetries(t *testing.T) {
	defaults := redis.NewClient(&redis.Options{}).Options()
	assert.Equal(t, time.Minute, pushTTL(defaults), "the default retry window, about 35s, gets the floor")

	slow := redis.NewClient(&redis.Options{ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}).Options()
	window := 3 * (30*time.Second + 512*time.Millisecond + 31*time.Second + 30*time.Second) // pool wait is ReadTimeout + 1s
	assert.GreaterOrEqual(t, pushTTL(slow), window, fmt.Sprintf("%+v", slow))

	none := redis.NewClient(&redis.Options{MaxRetries: -1, ReadTimeout: -1}).Options()
	assert.Equal(t, time.Minute, pushTTL(none))
}
