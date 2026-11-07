package redisq

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
)

// Queue pause (spec/redis-storage.md 1.3.0, 6.7 and 6.8). While <base>:paused
// exists every pop hands out nothing (pausedOne, pausedBatch). Each Queue
// object caches the flag for varmq's scheduler (Paused): set by its own
// Pause and Resume, by a pop or sweep that finds the key, and by messages on
// <base>:control. The sweep reads the key every second, so a lost message
// costs at most a second.

// pausedOne and pausedBatch go after a pop's re-send guard, before it pops:
// KEYS[#KEYS - 2] is <base>:paused (see popKeys). A paused single pop
// returns 0, a paused batch pop {0, {}, 1}; neither joins the idle set.
const (
	pausedOne   = "if redis.call('EXISTS', KEYS[#KEYS - 2]) == 1 then return 0 end\n"
	pausedBatch = "if redis.call('EXISTS', KEYS[#KEYS - 2]) == 1 then return {0, {}, 1} end\n"
)

// pauseScript sets KEYS[1], :paused, to Redis's clock in ms unless it is
// set, and then tells KEYS[2], :control. Returns 1 when it set the key.
var pauseScript = redis.NewScript(deadlineFn + `local now = nowms()
if not redis.call('SET', KEYS[1], now, 'NX') then return 0 end
redis.call('PUBLISH', KEYS[2], 'paused:' .. now)
return 1`)

// resumeScript drops KEYS[1], :paused, wakes up to one idle consumer of
// KEYS[3] per item in queue KEYS[2] (length command ARGV[2], wake channel
// prefix ARGV[1]), and tells KEYS[4], :control. Returns 1 when it dropped
// the key.
var resumeScript = redis.NewScript(wakeFn + `if redis.call('DEL', KEYS[1]) == 0 then return 0 end
wake(KEYS[3], ARGV[1], redis.call(ARGV[2], KEYS[2]))
redis.call('PUBLISH', KEYS[4], 'resumed')
return 1`)

// pauseHooks are a Queue's OnPauseChange callbacks and what drives them:
// every change of Queue.paused, and every "paused:" message, kicks
// pauseLoop.
type pauseHooks struct {
	mu      sync.Mutex
	hooks   map[*func(bool)]struct{}
	kick    chan struct{} // buffer 1: a kick while one waits is the same kick
	loop    sync.Once     // starts pauseLoop on the first kick: no goroutine for a queue never paused
	recheck atomic.Bool   // a "paused:" message came: read the key
	// gen moves on at each Pause, Resume and control message. A pop or sweep
	// reply, which may arrive after one of them, sets the flag only if gen
	// did not move since its call went out (observe).
	gen atomic.Uint64
	// refusals counts pops refused for a pause (PauseRefusals).
	refusals atomic.Uint64
}

// Pause stops every consumer of the queue, in every process, taking items
// until Resume. Enqueue, Acknowledge, Reject and requeues work as before.
// The flag has no expiry and Purge keeps it. Idempotent: the first pause
// time is kept. It returns once Redis stored the flag.
func (q *Queue) Pause() error {
	if err := pauseScript.Run(q.ctx, q.client, []string{q.getPausedKey(), q.getControlKey()}).Err(); err != nil {
		return err
	}
	q.pauseHooks.gen.Add(1)
	q.setPaused(true)
	return nil
}

// Resume undoes Pause and wakes idle consumers, as an enqueue does.
// Idempotent.
func (q *Queue) Resume() error {
	keys := []string{q.getPausedKey(), q.queueKey, q.getIdleKey(), q.getControlKey()}
	if err := resumeScript.Run(q.ctx, q.client, keys, q.queueKey+":wake:", q.lenCmd()).Err(); err != nil {
		return err
	}
	q.pauseHooks.gen.Add(1)
	q.setPaused(false)
	return nil
}

// PauseRefusals counts this object's pops that Redis refused for a pause
// (varmq's optional QueuePauser method): a pop that came back empty while
// it did not move was not refused, so varmq can tell a refusal racing a
// Resume from a failed pop.
func (q *Queue) PauseRefusals() uint64 {
	return q.pauseHooks.refusals.Load()
}

// refused records a pop refused for a pause, read from a call that went
// out at gen g.
func (q *Queue) refused(g uint64) {
	q.pauseHooks.refusals.Add(1)
	q.observe(g, true)
}

// Paused reports whether the queue is paused, as this object last saw it.
// No I/O.
func (q *Queue) Paused() bool {
	return q.paused.Load()
}

// OnPauseChange calls fn with the state each time this object sees it change
// (its own Pause or Resume, a pop or sweep, a control message), until cancel.
// Calls come from one goroutine of this object, in order, so the last call
// is the current state; a slow fn delays only later calls.
func (q *Queue) OnPauseChange(fn func(paused bool)) (cancel func()) {
	h := &fn
	q.pauseHooks.mu.Lock()
	if q.pauseHooks.hooks == nil {
		q.pauseHooks.hooks = map[*func(bool)]struct{}{}
	}
	q.pauseHooks.hooks[h] = struct{}{}
	q.pauseHooks.mu.Unlock()

	return func() {
		q.pauseHooks.mu.Lock()
		delete(q.pauseHooks.hooks, h)
		q.pauseHooks.mu.Unlock()
	}
}

// observe applies p, read from Redis by a call that went out at gen g,
// unless a Pause, Resume or control message came since: then p may be older
// than what they set.
// ponytail: a change landing between the check and the set still loses; the
// next pop or sweep (<= 1 s) corrects it.
func (q *Queue) observe(g uint64, p bool) {
	if q.pauseHooks.gen.Load() == g {
		q.setPaused(p)
	}
}

// setPaused updates the cache and, on a change, has pauseLoop call the hooks.
func (q *Queue) setPaused(p bool) {
	if q.paused.Swap(p) != p {
		q.kickPause()
	}
}

func (q *Queue) kickPause() {
	q.pauseHooks.loop.Do(func() { go q.pauseLoop() })
	select {
	case q.pauseHooks.kick <- struct{}{}:
	default:
	}
}

// pauseLoop, one per Queue object from its first kick until Close, reads
// the key after a "paused:" message and calls the hooks whenever the state
// differs from what it last told them. Off the pub/sub reader and the
// sweep, so a slow hook never delays wakes or heartbeats.
func (q *Queue) pauseLoop() {
	told := false
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-q.pauseHooks.kick:
		}

		if q.pauseHooks.recheck.Swap(false) {
			g := q.pauseHooks.gen.Load()
			if n, err := q.client.Exists(q.ctx, q.getPausedKey()).Result(); err == nil && q.pauseHooks.gen.Load() == g {
				q.paused.Store(n == 1)
			}
		}

		now := q.paused.Load()
		if now == told {
			continue
		}
		told = now

		q.pauseHooks.mu.Lock()
		fns := make([]func(bool), 0, len(q.pauseHooks.hooks))
		for h := range q.pauseHooks.hooks {
			fns = append(fns, *h)
		}
		q.pauseHooks.mu.Unlock()

		for _, fn := range fns {
			fn(now)
		}
	}
}

// control handles a message on the queue's subscription and reports whether
// it was a control message. Messages are hints (spec 6.8), and an unknown
// one is ignored. "resumed" is taken as is: a wrong one costs one refused
// pop, which sets the flag back. "paused:" only makes pauseLoop read the
// key, so forged ones never stop this consumer.
func (q *Queue) control(msg *redis.Message) bool {
	if msg.Channel != q.getControlKey() {
		return false
	}

	switch {
	case msg.Payload == "resumed":
		q.pauseHooks.gen.Add(1)
		q.setPaused(false)
	case strings.HasPrefix(msg.Payload, "paused:"):
		q.pauseHooks.gen.Add(1)
		q.pauseHooks.recheck.Store(true)
		q.kickPause()
	}
	return true
}

// lenCmd is the queue's length command.
func (q *Queue) lenCmd() string {
	if q.isPriority {
		return "ZCARD"
	}
	return "LLEN"
}

// getPausedKey returns the Redis key that pauses the queue while it exists
func (q *Queue) getPausedKey() string {
	return q.pausedKey
}

// getControlKey returns the pub/sub channel of pause and resume messages
func (q *Queue) getControlKey() string {
	return q.controlKey
}
