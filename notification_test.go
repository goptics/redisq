package redisq

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWakeKey = "test_wake_queue"

// newTestNotification returns a started notification on testWakeKey and, for
// a consumer, a channel of the signals it receives.
func newTestNotification(t *testing.T, consumer bool) (*Notification, chan string) {
	opts, err := redis.ParseURL(getTestRedisURL())
	require.NoError(t, err)

	client := redis.NewClient(opts)
	n := newNotification(client, testWakeKey, "LLEN")
	got := make(chan string, 10)
	if consumer {
		n.Subscribe(func(action string) { got <- action })
	}
	n.Start()

	// The subscription is asynchronous; wait until Redis has it.
	require.NoError(t, n.pubsub.Ping(context.Background()))

	t.Cleanup(func() {
		n.Stop()
		client.Close()
	})

	return n, got
}

func resetWakeKeys(t *testing.T) {
	opts, err := redis.ParseURL(getTestRedisURL())
	require.NoError(t, err)

	client := redis.NewClient(opts)
	defer client.Close()
	require.NoError(t, client.Del(context.Background(), testWakeKey, testWakeKey+":idle").Err())
}

// signals returns how many signals arrive on got within d.
func signals(got chan string, d time.Duration) int {
	n := 0
	deadline := time.After(d)
	for {
		select {
		case action := <-got:
			if action == "enqueued" {
				n++
			}
		case <-deadline:
			return n
		}
	}
}

func TestWakeReachesOneIdleConsumerEach(t *testing.T) {
	resetWakeKeys(t)
	a, gotA := newTestNotification(t, true)
	b, gotB := newTestNotification(t, true)
	producer, _ := newTestNotification(t, false)

	assert.Equal(t, 0, a.len())
	assert.Equal(t, 0, b.len())

	producer.wake(1)
	producer.wake(1)
	producer.wake(1) // nobody idle left

	assert.Equal(t, 1, signals(gotA, 300*time.Millisecond))
	assert.Equal(t, 1, signals(gotB, 300*time.Millisecond))
}

func TestWakeSkipsBusyConsumers(t *testing.T) {
	resetWakeKeys(t)
	_, got := newTestNotification(t, true) // idle: just subscribed
	producer, _ := newTestNotification(t, false)

	producer.wake(1) // takes it out of the idle set: now busy
	producer.wake(1)
	assert.Equal(t, 1, signals(got, 300*time.Millisecond))
}

func TestWakeCount(t *testing.T) {
	resetWakeKeys(t)
	producer, _ := newTestNotification(t, false)
	var gots []chan string
	for range 3 {
		c, got := newTestNotification(t, true)
		c.len()
		gots = append(gots, got)
	}

	producer.wake(2)

	total := 0
	for _, got := range gots {
		total += signals(got, 200*time.Millisecond)
	}
	assert.Equal(t, 2, total)
}

func TestProducerNeverJoinsIdleSet(t *testing.T) {
	resetWakeKeys(t)
	producer, _ := newTestNotification(t, false)

	assert.Equal(t, 0, producer.len())
	n, err := producer.client.SCard(context.Background(), producer.idleKey).Result()
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestWakeSkipsGoneConsumer(t *testing.T) {
	resetWakeKeys(t)
	a, got := newTestNotification(t, true)
	ctx := context.Background()
	require.NoError(t, a.client.SRem(ctx, a.idleKey, a.id).Err()) // a is busy

	// Only a gone consumer idle: it is dropped and nobody is woken.
	require.NoError(t, a.client.SAdd(ctx, a.idleKey, "crashed-instance").Err())
	a.wake(1)
	n, err := a.client.SCard(ctx, a.idleKey).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "the gone consumer should be dropped from the idle set")

	// A gone and a live consumer idle: whichever SPOP picks first, the live
	// one gets the wake.
	require.NoError(t, a.client.SAdd(ctx, a.idleKey, "crashed-instance").Err())
	a.len()
	a.wake(1)
	assert.Equal(t, 1, signals(got, 300*time.Millisecond))
}

func TestStopLeavesIdleSet(t *testing.T) {
	resetWakeKeys(t)
	a, got := newTestNotification(t, true)
	a.len()
	a.Stop()

	member, err := a.client.SIsMember(context.Background(), a.idleKey, a.id).Result()
	require.NoError(t, err)
	assert.False(t, member)

	a.wake(1)
	assert.Zero(t, signals(got, 200*time.Millisecond))
}

// Items that arrive without a wake (an older producer, another client) reach
// an idle consumer within about a second.
func TestNudgeWakesIdleConsumerWithWaitingItems(t *testing.T) {
	resetWakeKeys(t)
	a, got := newTestNotification(t, true)
	a.len()

	require.NoError(t, a.client.RPush(context.Background(), testWakeKey, "job").Err())
	assert.Equal(t, 1, signals(got, 2500*time.Millisecond))

	member, err := a.client.SIsMember(context.Background(), a.idleKey, a.id).Result()
	require.NoError(t, err)
	assert.False(t, member, "a nudged consumer is no longer idle")
}

// A consumer that subscribes while items already wait must be woken without
// any new enqueue.
func TestSubscribeOnBacklogGetsWoken(t *testing.T) {
	resetWakeKeys(t)
	producer, _ := newTestNotification(t, false)
	require.NoError(t, producer.client.RPush(context.Background(), testWakeKey, "a", "b").Err())

	_, got := newTestNotification(t, true)
	assert.Equal(t, 1, signals(got, 2500*time.Millisecond))
}
