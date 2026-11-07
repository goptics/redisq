package redisq

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pauseClient is a client of its own, as another process would have.
func pauseClient(t *testing.T) *queues {
	t.Helper()
	qs := New(getTestRedisURL()).(*queues)
	t.Cleanup(func() { qs.Close() })
	return qs
}

// pausable is what every queue kind serves for the pause tests.
type pausable interface {
	Pause() error
	Resume() error
	Paused() bool
	Len() int
	Dequeue() (any, bool)
	DequeueWithAckId() (any, bool, string)
	DequeueBatchWithAckIds(items []any, ackIDs []string) (n, remaining int, err error)
	QueueStats(ctx context.Context) (contract.QueueStats, error)
}

// A paused queue hands out nothing through any pop; adds still land;
// QueueStats reads the pause from Redis; Resume serves again.
func TestPauseGatesEveryPop(t *testing.T) {
	qs := pauseClient(t)
	key := newID()
	fifo, prio := qs.NewQueue("fifo-"+key), qs.NewPriorityQueue("prio-"+key)
	dfifo, dprio := qs.NewDistributedQueue("dfifo-"+key), qs.NewDistributedPriorityQueue("dprio-"+key)
	kinds := map[string]struct {
		q   pausable
		add func(string) bool
	}{
		"fifo":                 {fifo, func(s string) bool { return fifo.Enqueue(s) }},
		"priority":             {prio, func(s string) bool { return prio.Enqueue(s, 1) }},
		"distributed":          {dfifo, func(s string) bool { return dfifo.Enqueue(s) }},
		"distributed priority": {dprio, func(s string) bool { return dprio.Enqueue(s, 1) }},
	}

	for name, k := range kinds {
		t.Run(name, func(t *testing.T) {
			q := k.q
			require.NoError(t, q.Pause())
			assert.True(t, q.Paused())
			for i := range 3 {
				require.True(t, k.add(fmt.Sprint(i)), "adds land while paused")
			}
			assert.Equal(t, 3, q.Len())

			_, ok := q.Dequeue()
			assert.False(t, ok)
			_, ok, _ = q.DequeueWithAckId()
			assert.False(t, ok)
			items, ids := make([]any, 2), make([]string, 2)
			n, remaining, err := q.DequeueBatchWithAckIds(items, ids)
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.Zero(t, remaining, "a paused pop reports nothing left")

			s, err := q.QueueStats(context.Background())
			require.NoError(t, err)
			assert.True(t, s.Paused)
			at := s.PausedAt
			assert.NotZero(t, at)
			require.NoError(t, q.Pause())
			s, _ = q.QueueStats(context.Background())
			assert.Equal(t, at, s.PausedAt, "pausing again keeps the first time")

			require.NoError(t, q.Resume())
			assert.False(t, q.Paused())
			s, _ = q.QueueStats(context.Background())
			assert.False(t, s.Paused)
			assert.Zero(t, s.PausedAt)
			_, ok = q.Dequeue()
			assert.True(t, ok)
			_, ok, _ = q.DequeueWithAckId()
			assert.True(t, ok)
			n, _, err = q.DequeueBatchWithAckIds(items, ids)
			require.NoError(t, err)
			assert.Equal(t, 1, n)
		})
	}
}

// A paused pop still hands back what an earlier run of the same call popped
// (a go-redis re-send), and a refused pop leaves no reservation behind.
func TestPauseGuardBeforeGate(t *testing.T) {
	q := pauseClient(t).NewQueue(newID())
	for i := range 3 {
		require.True(t, q.Enqueue(fmt.Sprint(i)))
	}

	// One batch call, run twice with the same reservation, a Pause between.
	resv := fmt.Sprintf("%020d", q.ackSeq.Add(1))
	run := func() []any {
		r, err := listAckPop.Run(q.ctx, q.client, q.popKeys(), 2, q.vt(), q.ackPrefix, resv, "", 2, q.hbt(), q.ackPrefix).Slice()
		require.NoError(t, err)
		return r
	}
	first := run()
	require.Len(t, first[1], 2)
	require.NoError(t, q.Pause())
	again := run()
	assert.Len(t, again, 2, "a hand-back, not a paused reply")
	assert.Equal(t, first[1], again[1])

	// Single pop: same rule.
	ackID := q.ackIDOf(fmt.Sprintf("%020d", q.ackSeq.Add(1)), 1)
	require.NoError(t, q.Resume())
	one := func() any {
		v, err := listAckPopOne.Run(q.ctx, q.client, q.popKeys(), q.vt(), ackID, "", q.hbt(), q.ackPrefix).Result()
		require.NoError(t, err)
		return v
	}
	v := one()
	require.Equal(t, "2", v)
	require.NoError(t, q.Pause())
	assert.Equal(t, "2", one())

	// A refused call through the API keeps no reservation.
	_, ok, _ := q.DequeueWithAckId()
	assert.False(t, ok)
	n, _, err := q.DequeueBatchWithAckIds(make([]any, 2), make([]string, 2))
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Nil(t, q.retryPop.Load(), "no retryPop leak")
}

// Another process sees a Pause and a Resume within milliseconds (control
// channel) and calls its hooks; a later object starts paused.
func TestPauseSeenByOtherProcesses(t *testing.T) {
	key := newID()
	a := pauseClient(t).NewQueue(key)
	b := pauseClient(t).NewDistributedQueue(key)
	var mu sync.Mutex
	var calls []bool
	got := func() []bool { mu.Lock(); defer mu.Unlock(); return append([]bool(nil), calls...) }
	b.OnPauseChange(func(p bool) {
		mu.Lock()
		calls = append(calls, p)
		mu.Unlock()
	})

	require.NoError(t, a.Pause())
	require.Eventually(t, b.Paused, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(got()) == 1 }, time.Second, time.Millisecond, "hooks run off the reader")
	later := pauseClient(t).NewQueue(key)
	assert.True(t, later.Paused(), "a later object reads the key before its first pop")

	require.NoError(t, a.Resume())
	require.Eventually(t, func() bool { return !b.Paused() }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(got()) == 2 }, time.Second, time.Millisecond)
	assert.Equal(t, []bool{true, false}, got())
}

// A pause set or cleared without a control message (lost, or another client)
// reaches the cache within a sweep.
func TestPauseLostControlHeals(t *testing.T) {
	q := pauseClient(t).NewQueue(newID())
	require.NoError(t, q.client.Set(q.ctx, q.getPausedKey(), "1", 0).Err())
	require.Eventually(t, q.Paused, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, q.client.Del(q.ctx, q.getPausedKey()).Err())
	require.Eventually(t, func() bool { return !q.Paused() }, 2*time.Second, 10*time.Millisecond)

	s, err := q.QueueStats(context.Background())
	require.NoError(t, err)
	assert.False(t, s.Paused)
	require.NoError(t, q.client.Set(q.ctx, q.getPausedKey(), "not-ms", 0).Err())
	s, err = q.QueueStats(context.Background())
	require.NoError(t, err)
	assert.True(t, s.Paused)
	assert.Zero(t, s.PausedAt, "not an integer: no paused_at")
}

// Resume wakes an idle distributed consumer, as an enqueue does.
func TestResumeWakesIdleConsumer(t *testing.T) {
	q := pauseClient(t).NewDistributedQueue(newID())
	woken := make(chan string, 10)
	q.Subscribe(func(a string) { woken <- a })
	require.NoError(t, q.Pause())
	require.True(t, q.Queue.Enqueue("x")) // a plain push: no enqueue wake
	assert.Equal(t, 1, q.Len())

	require.NoError(t, q.Queue.client.SAdd(q.Queue.ctx, q.idleKey, q.id).Err()) // idle, as after an empty pop
	require.NoError(t, q.Resume())
	select {
	case a := <-woken:
		assert.Equal(t, "enqueued", a)
	case <-time.After(time.Second):
		t.Fatal("no wake on Resume")
	}
}

// The sweep still requeues and calls onRequeue on an unpaused queue (its
// reply grew a third element).
func TestPauseSweepStillRequeues(t *testing.T) {
	q := pauseClient(t).NewQueue(newID())
	q.SetVisibilityTimeout(time.Millisecond)
	var requeued atomic.Int64
	q.setOnRequeue(func(n int64) { requeued.Add(n) })
	require.True(t, q.Enqueue("x"))
	_, ok, _ := q.DequeueWithAckId()
	require.True(t, ok)
	time.Sleep(10 * time.Millisecond)
	require.True(t, q.tick())
	assert.EqualValues(t, 1, requeued.Load())
	assert.False(t, q.Paused())
}

// A "paused:" message only makes the object read the key: forged ones,
// however many, never pause it.
func TestPauseForgedMessageIgnored(t *testing.T) {
	q := pauseClient(t).NewQueue(newID())
	for range 50 {
		require.NoError(t, q.client.Publish(q.ctx, q.getControlKey(), "paused:1").Err())
	}
	assert.Never(t, q.Paused, 300*time.Millisecond, time.Millisecond)
}

// holdTick holds the reply of the next sweep script call until release.
type holdTick struct {
	hash    string
	armed   atomic.Bool
	ran     chan struct{}
	release chan struct{}
}

func (h *holdTick) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *holdTick) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *holdTick) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if a := cmd.Args(); len(a) > 1 && a[0] == "evalsha" && a[1] == h.hash && h.armed.CompareAndSwap(true, false) {
			h.ran <- struct{}{}
			<-h.release
		}
		return err
	}
}

// A sweep reply read before a Pause or Resume does not undo it.
func TestPauseStaleSweepIgnored(t *testing.T) {
	for _, pause := range []bool{true, false} {
		q := pauseClient(t).NewQueue(newID())
		if !pause {
			require.NoError(t, q.Pause())
		}
		h := &holdTick{hash: q.tickScript.Hash(), ran: make(chan struct{}), release: make(chan struct{})}
		q.client.AddHook(h)
		h.armed.Store(true)
		done := make(chan struct{})
		go func() { q.tick(); close(done) }()
		<-h.ran // the sweep read the flag; its reply waits

		if pause {
			require.NoError(t, q.Pause())
		} else {
			require.NoError(t, q.Resume())
		}
		close(h.release)
		<-done
		assert.Equal(t, pause, q.Paused(), "pause=%v", pause)
	}
}

// A hook stops after cancel; storage errors reach the caller.
func TestPauseCancelAndErrors(t *testing.T) {
	qs := pauseClient(t)
	q := qs.NewQueue(newID())
	var calls atomic.Int32
	cancel := q.OnPauseChange(func(bool) { calls.Add(1) })
	require.NoError(t, q.Pause())
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	cancel()
	require.NoError(t, q.Resume())
	assert.Never(t, func() bool { return calls.Load() > 1 }, 100*time.Millisecond, time.Millisecond, "no call after cancel")

	require.NoError(t, qs.client.Close())
	assert.Error(t, q.Pause())
	assert.Error(t, q.Resume())
	_, ok := q.Dequeue()
	assert.False(t, ok)
}
