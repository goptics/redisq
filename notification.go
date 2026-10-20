package redisq

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucsky/cuid"
	"github.com/redis/go-redis/v9"
)

// Notification wakes consumers of a distributed queue. A busy consumer keeps
// pulling on its own, so only idle ones need a signal: a consumer that finds
// the queue empty joins the idle set (<key>:idle), and each enqueue or
// requeue wakes at most one member, on that member's own channel
// (<key>:wake:<id>). With every consumer busy, nothing is published.
//
// An instance counts as a consumer once Subscribe is called; a producer-only
// instance never joins the idle set.
type Notification struct {
	client      *redis.Client
	pubsub      *redis.PubSub
	id          string
	queueKey    string
	lenCmd      string // LLEN or ZCARD
	idleKey     string
	wakePrefix  string
	consumer    atomic.Bool
	subscribers []func(action string)
	mx          sync.RWMutex
	started     sync.Once
	ctx         context.Context
	cancel      context.CancelFunc
}

// lenIdle returns the queue length (command ARGV[1]) and adds consumer
// ARGV[2] to the idle set when the queue is empty, in one step, so an
// enqueue either lands before the check or finds the consumer idle.
var lenIdle = redis.NewScript(`local n = redis.call(ARGV[1], KEYS[1])
if n == 0 and ARGV[2] ~= '' then redis.call('SADD', KEYS[2], ARGV[2]) end
return n`)

// wakeFn defines wake(idle, prefix, count) for scripts: it publishes
// "enqueued" to up to count idle consumers, taking each out of the idle set.
// A consumer that is gone has no subscriber, so PUBLISH reaches nobody and the
// next one is tried.
const wakeFn = `local function wake(idle, prefix, count)
	local woken = 0
	while woken < count do
		local id = redis.call('SPOP', idle)
		if not id then break end
		if redis.call('PUBLISH', prefix .. id, 'enqueued') > 0 then woken = woken + 1 end
	end
	return woken
end
`

// wakeIdle wakes up to ARGV[2] idle consumers of idle set KEYS[1].
var wakeIdle = redis.NewScript(wakeFn + `return wake(KEYS[1], ARGV[1], tonumber(ARGV[2]))`)

// nudge brings consumer ARGV[2]'s idle-set membership in line with the
// queue and returns its length: with the queue empty the consumer is (back)
// in the idle set, with items waiting it leaves it, since it is about to be
// poked to pull them.
var nudge = redis.NewScript(`local n = redis.call(ARGV[1], KEYS[1])
if n == 0 then redis.call('SADD', KEYS[2], ARGV[2]) else redis.call('SREM', KEYS[2], ARGV[2]) end
return n`)

func newNotification(client *redis.Client, queueKey, lenCmd string) *Notification {
	ctx, cancel := context.WithCancel(context.Background())
	id := cuid.New()
	wakePrefix := queueKey + ":wake:"

	return &Notification{
		client:      client,
		pubsub:      client.Subscribe(ctx, wakePrefix+id),
		id:          id,
		queueKey:    queueKey,
		lenCmd:      lenCmd,
		idleKey:     queueKey + ":idle",
		wakePrefix:  wakePrefix,
		subscribers: make([]func(action string), 0),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Start delivers wake signals to the subscribers and runs the nudge loop.
// The distributed queue constructors call it; later calls, as older docs
// showed after Subscribe, do nothing instead of starting a second reader and
// nudge loop.
func (n *Notification) Start() {
	n.started.Do(func() {
		go func() {
			for msg := range n.pubsub.Channel() {
				if msg == nil {
					continue
				}
				n.broadcast(msg.Payload)
			}
		}()

		go n.nudgeLoop()
	})
}

// nudgeLoop pokes this consumer once a second while items wait, and puts it
// back in the idle set while the queue is empty. Wakes can be lost: one can
// reach a consumer that can't take work (paused, or full but marked idle by
// a Len call from elsewhere), a consumer whose pub/sub connection was
// reconnecting is dropped from the idle set as gone, and items pushed by
// older versions or other clients send none. A busy consumer just ignores
// the poke.
// ponytail: one script call per consumer per second.
func (n *Notification) nudgeLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-n.ctx.Done():
			return
		case <-tick.C:
		}

		if !n.consumer.Load() {
			continue
		}

		waiting, err := nudge.Run(n.ctx, n.client, []string{n.queueKey, n.idleKey}, n.lenCmd, n.id).Int()
		if errors.Is(err, redis.ErrClosed) {
			return // the client was closed without closing this queue
		}

		if waiting > 0 {
			n.broadcast("enqueued")
		}
	}
}

func (n *Notification) broadcast(msg string) {
	n.mx.RLock()
	defer n.mx.RUnlock()

	for _, subscriber := range n.subscribers {
		subscriber(msg)
	}
}

// Subscribe registers handler for wake signals ("enqueued") and makes this
// instance a consumer. A consumer that just subscribed has no work yet, so
// it joins the idle set: an enqueue wakes it, and so does the nudge if items
// were already waiting (varmq sends no signal when it binds a queue).
func (n *Notification) Subscribe(handler func(action string)) {
	n.mx.Lock()
	n.subscribers = append(n.subscribers, handler)
	n.mx.Unlock()

	n.consumer.Store(true)
	n.markIdle()
}

// len returns the queue length, marking this consumer idle when it is 0.
func (n *Notification) len() int {
	id := ""
	if n.consumer.Load() {
		id = n.id
	}

	length, err := lenIdle.Run(n.ctx, n.client, []string{n.queueKey, n.idleKey}, n.lenCmd, id).Int()
	if err != nil {
		return 0
	}
	return length
}

// consumerID is this instance's ID if it is a consumer, else ”.
func (n *Notification) consumerID() string {
	if n.consumer.Load() {
		return n.id
	}
	return ""
}

// markIdle adds this consumer to the idle set after a dequeue found nothing.
// An enqueue between the miss and this call sends no wake; nudgeLoop catches
// it.
func (n *Notification) markIdle() {
	if n.consumer.Load() {
		n.client.SAdd(n.ctx, n.idleKey, n.id)
	}
}

// wake signals up to count idle consumers, one per new item.
func (n *Notification) wake(count int64) {
	if err := wakeIdle.Run(n.ctx, n.client, []string{n.idleKey}, n.wakePrefix, count).Err(); err != nil {
		log.Printf("Error waking a consumer: %v", err)
	}
}

func (n *Notification) Stop() {
	n.client.SRem(context.Background(), n.idleKey, n.id)
	n.cancel()
	n.pubsub.Close()

	n.mx.Lock()
	n.subscribers = nil
	n.mx.Unlock()
}
