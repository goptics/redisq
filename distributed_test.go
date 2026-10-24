package redisq

import (
	"context"
	"crypto/sha1"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDistributedQueue(t *testing.T) (*DistributedQueue, func()) {
	redisURL := getTestRedisURL()
	qs := New(redisURL)
	q := qs.NewDistributedQueue(testQueueKey)

	// Clear the queue before test
	ctx := context.Background()
	require.NoError(t, q.Queue.client.Del(ctx, testQueueKey).Err(), "Failed to clear test queue")

	cleanup := func() {
		q.Close() // first: it requeues what the test left pending
		q.Queue.client.Del(ctx, testQueueKey)
		qs.Close()
	}

	return q, cleanup
}

func TestNewDistributedQueue(t *testing.T) {
	q, cleanup := setupTestDistributedQueue(t)
	defer cleanup()

	assert.NotNil(t, q.Queue, "Queue should not be nil")
	assert.NotNil(t, q.Notification, "Notification should not be nil")
}

func TestDistributedQueueEnqueueDequeue(t *testing.T) {
	q, cleanup := setupTestDistributedQueue(t)
	defer cleanup()

	notifications := make(chan string, 2)
	q.Subscribe(func(action string) {
		notifications <- action
	})
	assert.Equal(t, 0, q.Len(), "an empty queue marks the consumer idle")

	// Test Enqueue with notification
	assert.True(t, q.Enqueue("test data"))
	select {
	case action := <-notifications:
		assert.Equal(t, "enqueued", action)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for enqueue notification")
	}

	// Test Dequeue with notification
	data, ok := q.Dequeue()
	assert.True(t, ok)
	assert.Equal(t, []byte("test data"), data)

	select {
	case action := <-notifications:
		t.Fatalf("unexpected %q notification after dequeue", action)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDistributedQueueConcurrency(t *testing.T) {
	q, cleanup := setupTestDistributedQueue(t)
	defer cleanup()

	const numGoroutines = 10
	const numOperations = 100

	notifications := make(chan string, numGoroutines*numOperations)
	q.Subscribe(func(action string) {
		notifications <- action
	})
	assert.Equal(t, 0, q.Len())
	before := publishes(t, q.Queue.client)

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Enqueue concurrently
	for range numGoroutines {
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				assert.True(t, q.Enqueue([]byte("test")))
			}
		}()
	}

	wg.Wait()

	// One idle consumer gets one wake for the whole burst, not one per item:
	// once woken it keeps pulling on its own.
	assert.Equal(t, 1, publishes(t, q.Queue.client)-before)
	assert.Equal(t, numGoroutines*numOperations, q.Len())
}

// A job whose consumer never acknowledges it must come back on its own, and
// wake distributed workers, without waiting for a process restart.
func TestDistributedQueueSweeperRequeues(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()

	key := "test_sweeper_queue"
	dq := qs.NewDistributedQueue(key)
	defer dq.Close()
	defer dq.Queue.client.Del(context.Background(), key, dq.getNackedItemKey(), dq.getTimeoutKey())

	enqueued := make(chan struct{}, 10)
	dq.Subscribe(func(action string) {
		if action == "enqueued" {
			enqueued <- struct{}{}
		}
	})

	dq.SetVisibilityTimeout(time.Second)
	dq.Len() // idle
	require.True(t, dq.Enqueue("job"))
	<-enqueued

	_, ok, _ := dq.DequeueWithAckId()
	require.True(t, ok)
	dq.Len() // the consumer's loop finds the queue empty again: idle

	select {
	case <-enqueued:
	case <-time.After(4 * time.Second):
		t.Fatal("sweeper did not requeue the expired job")
	}
	assert.Equal(t, 1, dq.Len())
	assert.Equal(t, 0, dq.GetNackedItemsCount())
}

// cmdCounter counts the commands a client sends, except the background
// sweep and nudge scripts.
type cmdCounter struct {
	mx   sync.Mutex
	n    int
	skip map[string]bool // script hashes
}

func newCmdCounter() *cmdCounter {
	return &cmdCounter{skip: map[string]bool{
		nudge.Hash(): true, listRequeue.Hash(): true, zsetRequeue.Hash(): true,
		listTick.Hash(): true, zsetTick.Hash(): true,
	}}
}

func (c *cmdCounter) record(cmd redis.Cmder) {
	args := cmd.Args()
	if len(args) > 1 {
		switch cmd.Name() {
		case "evalsha":
			if c.skip[fmt.Sprint(args[1])] {
				return
			}
		case "eval":
			if c.skip[fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprint(args[1]))))] {
				return
			}
		}
	}
	c.mx.Lock()
	c.n++
	c.mx.Unlock()
}

func (c *cmdCounter) count() int {
	c.mx.Lock()
	defer c.mx.Unlock()
	return c.n
}

func (c *cmdCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *cmdCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.record(cmd)
		return next(ctx, cmd)
	}
}

func (c *cmdCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			c.record(cmd)
		}
		return next(ctx, cmds)
	}
}

// An enqueue on a distributed queue is one round trip: the push and the wake
// run in one script.
func TestDistributedEnqueueIsOneRoundTrip(t *testing.T) {
	const n = 100

	t.Run("list", func(t *testing.T) {
		q, cleanup := setupTestDistributedQueue(t)
		defer cleanup()
		q.SetExpiration(time.Minute)
		require.True(t, q.Enqueue("warm-up")) // loads the script

		c := newCmdCounter()
		q.Queue.client.AddHook(c)
		for i := range n {
			require.True(t, q.Enqueue(fmt.Sprint(i)))
		}
		assert.Equal(t, n, c.count())
	})

	t.Run("priority", func(t *testing.T) {
		q, cleanup := setupTestDistributedPriorityQueue(t)
		defer cleanup()
		q.SetExpiration(time.Minute)
		require.True(t, q.Enqueue("warm-up", 1))

		c := newCmdCounter()
		q.Queue.client.AddHook(c)
		for i := range n {
			require.True(t, q.Enqueue(fmt.Sprint(i), i))
		}
		assert.Equal(t, n, c.count())
	})
}

// An enqueue onto a backlog wakes every idle consumer, not just one, so they
// share the backlog at once instead of waiting for the nudge.
func TestEnqueueOntoBacklogWakesAllIdle(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	key := "test_backlog_wake"
	ctx := context.Background()

	var consumers []chan string
	for range 3 {
		c := qs.NewDistributedQueue(key)
		defer c.Close()
		got := make(chan string, 10)
		c.Subscribe(func(action string) { got <- action }) // idle
		consumers = append(consumers, got)
	}
	producer := qs.NewDistributedQueue(key)
	defer producer.Close()
	defer producer.Queue.client.Del(ctx, key, key+":idle")

	require.NoError(t, producer.Queue.client.RPush(ctx, key, "a", "b", "c", "d").Err()) // no wake
	require.True(t, producer.Enqueue("e"))

	for i, got := range consumers {
		select {
		case <-got:
		case <-time.After(300 * time.Millisecond):
			t.Fatalf("consumer %d was not woken", i)
		}
	}
}

// A consumer whose dequeue finds nothing joins the idle set, so the next
// enqueue wakes it.
func TestEmptyDequeueMarksConsumerIdle(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	ctx := context.Background()

	dq := qs.NewDistributedQueue("test_empty_dequeue_list")
	defer dq.Close()
	dpq := qs.NewDistributedPriorityQueue("test_empty_dequeue_prio")
	defer dpq.Close()

	for _, c := range []struct {
		n       *Notification
		dequeue func() bool
	}{
		{dq.Notification, func() bool { _, ok := dq.Dequeue(); return ok }},
		{dq.Notification, func() bool { _, ok, _ := dq.DequeueWithAckId(); return ok }},
		{dpq.Notification, func() bool { _, ok := dpq.Dequeue(); return ok }},
		{dpq.Notification, func() bool { _, ok, _ := dpq.DequeueWithAckId(); return ok }},
	} {
		c.n.Subscribe(func(string) {})
		require.NoError(t, c.n.client.SRem(ctx, c.n.idleKey, c.n.id).Err())

		assert.False(t, c.dequeue())
		member, err := c.n.client.SIsMember(ctx, c.n.idleKey, c.n.id).Result()
		require.NoError(t, err)
		assert.True(t, member, "%s: empty dequeue should mark the consumer idle", c.n.queueKey)
	}
}

// Enqueue refuses work once the queue or its client is closed, and Len
// reads 0 instead of failing.
func TestDistributedQueueClosed(t *testing.T) {
	qs := New(getTestRedisURL())
	dq := qs.NewDistributedQueue("test_closed_dq")
	dpq := qs.NewDistributedPriorityQueue("test_closed_dpq")
	dpq.Subscribe(func(string) {}) // a consumer, so its nudge loop runs

	require.NoError(t, dq.Close())
	assert.False(t, dq.Enqueue("job"), "enqueue on a closed queue")

	require.NoError(t, qs.Close())
	assert.False(t, dpq.Enqueue("job", 1), "enqueue on a closed client")
	assert.Equal(t, 0, dpq.Len())

	// The nudge loop must stop on the closed client rather than spin.
	time.Sleep(1200 * time.Millisecond)
	_ = dpq.Close()
}

// distQueue is what the heartbeat tests use of both distributed queue types.
type distQueue interface {
	Subscribe(handler func(action string))
	DequeueWithAckId() (any, bool, string)
	Len() int
	Close() error
}

// heartbeatCases open a distributed queue of each storage on key, with its
// push (priority 3).
var heartbeatCases = []struct {
	name string
	open func(qs Queues, key string) (distQueue, *Queue, func(string) bool)
}{
	{"list", func(qs Queues, key string) (distQueue, *Queue, func(string) bool) {
		q := qs.NewDistributedQueue(key)
		return q, q.Queue, func(v string) bool { return q.Enqueue(v) }
	}},
	{"priority", func(qs Queues, key string) (distQueue, *Queue, func(string) bool) {
		q := qs.NewDistributedPriorityQueue(key)
		return q, q.Queue, func(v string) bool { return q.Enqueue(v, 3) }
	}},
}

// wakes collects a consumer's wake signals.
func wakes(q distQueue) chan struct{} {
	ch := make(chan struct{}, 100)
	q.Subscribe(func(string) { ch <- struct{}{} })
	return ch
}

// A consumer busy with a job stays out of the idle set while the queue sits
// empty, so the next enqueue wakes a free consumer at once instead of the
// busy one, which would ignore the wake and leave the free one to its nudge.
func TestBusyConsumerStaysOutOfIdleSet(t *testing.T) {
	for _, c := range heartbeatCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			key := "test_busy_not_idle_" + c.name
			qs := New(getTestRedisURL())
			defer qs.Close()
			c1, q1, push := c.open(qs, key)
			delKeys(t, q1.client, key)
			defer delKeys(t, q1.client, key)
			defer c1.Close()
			wakes(c1)

			require.True(t, push("x"))
			_, ok, _ := c1.DequeueWithAckId() // busy with x from here on
			require.True(t, ok)

			c2, q2, _ := c.open(qs, key)
			defer c2.Close()
			free := wakes(c2)

			time.Sleep(1500 * time.Millisecond) // c1's nudge runs on the empty queue
			idle, err := q1.client.SMembers(ctx, key+":idle").Result()
			require.NoError(t, err)
			assert.Equal(t, []string{q2.ackPrefix}, idle, "only the free consumer is idle")

			require.True(t, push("y"))
			select {
			case <-free:
			case <-time.After(300 * time.Millisecond):
				t.Fatal("the free consumer was not woken at once")
			}
		})
	}
}

// A batch pop that fills every slot leaves the consumer busy, so its nudge
// won't put it back in the idle set; one with a slot to spare leaves it idle.
func TestBatchDequeueSetsIdle(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	dq := qs.NewDistributedQueue("test_batch_idle_list")
	dpq := qs.NewDistributedPriorityQueue("test_batch_idle_prio")

	for _, c := range []struct {
		q    distQueue
		n    *Notification
		push func() bool
		pop  func([]any, []string) (int, int, error)
	}{
		{dq, dq.Notification, func() bool { return dq.Enqueue("x") }, dq.DequeueBatchWithAckIds},
		{dpq, dpq.Notification, func() bool { return dpq.Enqueue("x", 1) }, dpq.DequeueBatchWithAckIds},
	} {
		delKeys(t, c.n.client, c.n.queueKey)
		c.q.Subscribe(func(string) {})
		for _, slots := range []int{1, 2} {
			require.True(t, c.push())
			n, _, err := c.pop(make([]any, slots), make([]string, slots))
			require.NoError(t, err)
			require.Equal(t, 1, n)
			assert.Equal(t, slots > 1, c.n.idle.Load(), "%s, %d slots", c.n.queueKey, slots)
		}
		require.NoError(t, c.q.Close())
		delKeys(t, c.n.client, c.n.queueKey)
	}
}

// A consumer that closes with work it has not acknowledged hands it back at
// once, like a closed RabbitMQ channel, and wakes another idle consumer for it.
func TestCloseRequeuesUnacked(t *testing.T) {
	for _, c := range heartbeatCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			key := "test_close_requeues_" + c.name
			qs1, qs2 := New(getTestRedisURL()), New(getTestRedisURL())
			defer qs1.Close()
			defer qs2.Close()
			c1, q1, push := c.open(qs1, key)
			delKeys(t, q1.client, key)
			defer delKeys(t, q1.client, key)
			c2, q2, _ := c.open(qs2, key)
			defer c2.Close()
			wakes(c1)

			require.True(t, push("job"))
			_, ok, _ := c1.DequeueWithAckId()
			require.True(t, ok)
			assert.Zero(t, c1.Len()) // idle: a Close that didn't leave first could wake itself

			// Another idle consumer, without the nudge loop that wakes it anyway.
			watcher := q2.client.Subscribe(ctx, key+":wake:watcher")
			defer watcher.Close()
			_, err := watcher.Receive(ctx)
			require.NoError(t, err)
			require.NoError(t, q2.client.SAdd(ctx, q2.getIdleKey(), "watcher").Err())

			require.NoError(t, c1.Close())
			assert.Equal(t, 0, q2.GetNackedItemsCount(), "requeued by Close itself")
			wctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			_, err = watcher.ReceiveMessage(wctx)
			require.NoError(t, err, "closing consumer did not wake the other")
			assert.ErrorIs(t, q2.client.ZScore(ctx, q2.getHeartbeatKey(), q1.ackPrefix).Err(), redis.Nil, "heartbeat dropped")

			if c.name == "priority" {
				assert.Equal(t, "zset", q2.client.Type(ctx, key).Val())
				assert.Zero(t, q2.client.HLen(ctx, q2.getPriorityKey()).Val())
				members := q2.client.ZRangeWithScores(ctx, key, 0, -1).Val()
				require.Len(t, members, 1)
				assert.EqualValues(t, 3, members[0].Score, "priority kept")
			}
			item, ok, _ := c2.DequeueWithAckId()
			require.True(t, ok)
			assert.Equal(t, "job", string(item.([]byte)))
		})
	}
}

// A consumer that dies without Close (here its client is closed, which stops
// its sweep as a crash would) has its items requeued once its heartbeat runs
// out, long before the visibility timeout. A producer never gets a heartbeat.
func TestHeartbeatRunsOutRequeues(t *testing.T) {
	const ttl = 3 * time.Second
	for _, c := range heartbeatCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			key := "test_heartbeat_runs_out_" + c.name
			qs1, qs2 := New(getTestRedisURL()), New(getTestRedisURL())
			defer qs2.Close()
			c1, q1, push := c.open(qs1, key)
			defer c1.Close()
			delKeys(t, q1.client, key)
			c2, q2, _ := c.open(qs2, key)
			defer c2.Close()
			defer delKeys(t, q2.client, key)
			q1.heartbeatTimeout.Store(int64(ttl)) // below the setter's floor

			require.True(t, push("job"))
			_, ok, _ := c1.DequeueWithAckId()
			require.True(t, ok)
			require.NoError(t, qs1.Close())

			time.Sleep(ttl - time.Second)
			assert.Equal(t, 1, q2.GetNackedItemsCount(), "requeued before the heartbeat ran out")
			deadline := time.Now().Add(4 * time.Second)
			for c2.Len() == 0 {
				require.True(t, time.Now().Before(deadline), "dead consumer's item was not requeued")
				time.Sleep(50 * time.Millisecond)
			}
			item, ok, _ := c2.DequeueWithAckId()
			require.True(t, ok)
			assert.Equal(t, "job", string(item.([]byte)))
			assert.ErrorIs(t, q2.client.ZScore(ctx, q2.getHeartbeatKey(), q1.ackPrefix).Err(), redis.Nil, "dead heartbeat dropped")

			producer, pq, push2 := c.open(qs2, key)
			defer producer.Close()
			require.True(t, push2("more"))
			time.Sleep(1100 * time.Millisecond) // a sweep
			assert.ErrorIs(t, q2.client.ZScore(ctx, q2.getHeartbeatKey(), pq.ackPrefix).Err(), redis.Nil, "a producer got a heartbeat")
		})
	}
}

// A live consumer keeps its items with no visibility timeout, and a sweeper
// with none doesn't requeue every pending item as RequeueNackedItems does.
func TestHeartbeatKeepsLiveConsumersItems(t *testing.T) {
	const ttl = 3 * time.Second
	qs := New(getTestRedisURL())
	defer qs.Close()
	key := "test_heartbeat_live"
	holder := qs.NewDistributedQueue(key)
	defer holder.Close()
	delKeys(t, holder.Queue.client, key)
	defer delKeys(t, holder.Queue.client, key)
	sweeper := qs.NewDistributedQueue(key)
	defer sweeper.Close()
	holder.heartbeatTimeout.Store(int64(ttl))
	holder.SetVisibilityTimeout(0)
	sweeper.SetVisibilityTimeout(0)

	require.True(t, holder.Enqueue("job"))
	_, ok, _ := holder.DequeueWithAckId()
	require.True(t, ok)

	time.Sleep(2*ttl + time.Second)
	assert.Equal(t, 1, sweeper.GetNackedItemsCount())
	assert.Zero(t, sweeper.Len())
}

// A stall of Redis longer than the heartbeat timeout (a failover, a restart,
// a pause) doesn't make live consumers look dead when it ends: no one could
// beat during it.
func TestHeartbeatSurvivesRedisStall(t *testing.T) {
	const ttl, pause = 3 * time.Second, 4500 * time.Millisecond
	ctx := context.Background()
	key := "test_heartbeat_stall"
	qs1, qs2 := New(getTestRedisURL()), New(getTestRedisURL())
	defer qs1.Close()
	defer qs2.Close()
	a, b := qs1.NewDistributedQueue(key), qs2.NewDistributedQueue(key)
	defer a.Close()
	defer b.Close()
	delKeys(t, a.Queue.client, key)
	defer delKeys(t, a.Queue.client, key)
	for _, q := range []*DistributedQueue{a, b} {
		q.heartbeatTimeout.Store(int64(ttl))
		require.True(t, q.Enqueue("job"))
		_, ok, _ := q.DequeueWithAckId()
		require.True(t, ok)
	}
	time.Sleep(1500 * time.Millisecond) // the sweeps set the clock

	opts, err := redis.ParseURL(getTestRedisURL())
	require.NoError(t, err)
	admin := redis.NewClient(opts)
	defer admin.Close()
	require.NoError(t, admin.Do(ctx, "CLIENT", "PAUSE", pause.Milliseconds()).Err())
	time.Sleep(pause + ttl)

	assert.EqualValues(t, 2, admin.HLen(ctx, a.getNackedItemKey()).Val(), "a live consumer's item was requeued")
	assert.Zero(t, admin.LLen(ctx, key).Val())
	assert.EqualValues(t, 2, admin.ZCard(ctx, a.getHeartbeatKey()).Val())
}

// seen waits until a sweep has seen owner subscribed to its wake channel:
// with the clock just deleted, the second sweep from now.
func seen(t *testing.T, q *Queue, owner string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return q.client.ZScore(context.Background(), q.getPresenceKey(), owner).Err() == nil
	}, 3*time.Second, 20*time.Millisecond, "no sweep saw the consumer subscribed")
}

// A distributed consumer whose process dies (here its client closes, with its
// pub/sub connection, which also stops its sweep) has its items requeued a
// few seconds after Redis drops its subscription, long before its heartbeat
// runs out, as RabbitMQ requeues when a consumer's connection closes.
func TestGoneConsumerRequeues(t *testing.T) {
	for _, c := range heartbeatCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			key := "test_gone_consumer_" + c.name
			qs1, qs2 := New(getTestRedisURL()), New(getTestRedisURL())
			defer qs2.Close()
			c1, q1, push := c.open(qs1, key)
			defer c1.Close()
			delKeys(t, q1.client, key)
			c2, q2, _ := c.open(qs2, key)
			defer c2.Close()
			defer delKeys(t, q2.client, key)

			require.True(t, push("job"))
			_, ok, _ := c1.DequeueWithAckId()
			require.True(t, ok)
			seen(t, q2, q1.ackPrefix)
			require.NoError(t, qs1.Close())
			gone := time.Now()

			time.Sleep(presenceGrace - time.Second)
			assert.Equal(t, 1, q2.GetNackedItemsCount(), "requeued within the grace")
			for c2.Len() == 0 {
				require.Less(t, time.Since(gone), presenceGrace+3*time.Second, "gone consumer's item was not requeued")
				time.Sleep(50 * time.Millisecond)
			}
			item, ok, _ := c2.DequeueWithAckId()
			require.True(t, ok)
			assert.Equal(t, "job", string(item.([]byte)))
			assert.ErrorIs(t, q2.client.ZScore(ctx, q2.getPresenceKey(), q1.ackPrefix).Err(), redis.Nil, "presence dropped")
		})
	}
}

// A consumer whose subscription drops for less than the grace keeps its
// items, even with its sweep stalled (stopped here), so only the
// subscription speaks for it: first a go-redis reconnect after Redis kills
// the connection, then a gap of a few seconds. One that stays gone is taken
// for dead, the grace counting from when it went.
func TestPresenceGrace(t *testing.T) {
	ctx := context.Background()
	key := "test_presence_grace"
	qs := New(getTestRedisURL())
	defer qs.Close()
	c1, c2 := qs.NewDistributedQueue(key), qs.NewDistributedQueue(key)
	defer c2.Close()
	delKeys(t, c2.Queue.client, key)
	defer delKeys(t, c2.Queue.client, key)
	ch := c1.wakePrefix + c1.id
	subs := func() int64 { return c2.Queue.client.PubSubNumSub(ctx, ch).Val()[ch] }

	require.True(t, c1.Enqueue("job"))
	_, ok, _ := c1.DequeueWithAckId()
	require.True(t, ok)
	seen(t, c2.Queue, c1.ackPrefix)
	c1.Queue.cancel() // its sweep stops
	defer c1.Close()

	require.NoError(t, c2.Queue.client.Do(ctx, "CLIENT", "KILL", "TYPE", "pubsub").Err())
	require.Eventually(t, func() bool { return subs() == 1 }, 2*time.Second, 10*time.Millisecond, "go-redis resubscribed")

	require.NoError(t, c1.pubsub.Close())
	went := time.Now()
	time.Sleep(presenceGrace - 3*time.Second)
	back := c1.Queue.client.Subscribe(ctx, ch)
	_, err := back.Receive(ctx)
	require.NoError(t, err)
	time.Sleep(presenceGrace + 1500*time.Millisecond - time.Since(went))
	assert.Equal(t, 1, c2.GetNackedItemsCount(), "requeued after a short drop")

	require.NoError(t, back.Close())
	went = time.Now()
	for c2.GetNackedItemsCount() > 0 {
		require.Less(t, time.Since(went), presenceGrace+3*time.Second, "gone consumer's item was not requeued")
		time.Sleep(50 * time.Millisecond)
	}
	assert.Less(t, presenceGrace-time.Second, time.Since(went), "the grace counted again from the last drop")
	assert.EqualValues(t, 1, c2.Queue.client.LLen(ctx, key).Val())
}

// Close pushes an item back before it drops it from pending, so a push that
// fails (here the queue key holds a string) leaves the item pending.
func TestCloseKeepsItemWhenPushFails(t *testing.T) {
	for _, c := range retryCases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			q, push, cleanup := c.setup(t, getTestRedisURL())
			defer cleanup()
			delKeys(t, q.client, q.queueKey)
			defer delKeys(t, q.client, q.queueKey)
			require.True(t, push("a"))
			var ok bool
			if c.name == "priority" {
				_, ok, _ = (&PriorityQueue{Queue: q}).DequeueWithAckId()
			} else {
				_, ok, _ = q.DequeueWithAckId()
			}
			require.True(t, ok)

			require.NoError(t, q.client.Set(ctx, q.queueKey, "not a queue", 0).Err())
			assert.Error(t, q.Close())
			assert.EqualValues(t, 1, q.client.HLen(ctx, q.getNackedItemKey()).Val())
		})
	}
}
