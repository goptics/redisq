package redisq

import (
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
)

// Job cancel (spec/redis-storage.md 1.7.0, 6.14). CancelJob marks a Job ID
// in ZSET <base>:cancelled (score: when the mark expires, ms) and announces
// it on <base>:control. Pops drop a marked
// waiting job (cancelFn) with record and outcome cancelled; consumers learn
// of a running one (OnCancel) and finish it with AcknowledgeCancelled. Any
// finish or re-add drops the mark. Cooperative: storage never stops a
// running job itself.

var (
	_ contract.JobCanceller          = (*Queue)(nil)
	_ contract.JobCanceller          = (*PriorityQueue)(nil)
	_ contract.CancelNotifier        = (*Queue)(nil)
	_ contract.CancelNotifier        = (*PriorityQueue)(nil)
	_ contract.CancelAcknowledgeable = (*Queue)(nil)
	_ contract.CancelAcknowledgeable = (*PriorityQueue)(nil)
)

// cancelTTL is how long a cancel mark lasts. Each mark has its own expiry
// (its score); the key's PEXPIRE is only a backstop.
const cancelTTL = 24 * time.Hour

// cancelPrefix starts a cancel message on <base>:control.
const cancelPrefix = "cancel:"

var errEmptyJobID = errors.New("redisq: empty job id")

// cancelJobScript is CancelJob. KEYS: jobs, cancelled, control. ARGV: job
// id, mark TTL (ms). Returns 0, changing nothing, when the job's record
// shows it ended; else prunes expired marks, marks it until now + TTL
// (Redis's clock), sets the key's backstop TTL, announces it, and returns 1.
var cancelJobScript = redis.NewScript(deadlineFn + `local rec = redis.call('HGET', KEYS[1], ARGV[1])
if rec then
	local ok, r = pcall(cjson.decode, rec)
	if ok and type(r) == 'table' and (r.state == 'completed' or r.state == 'failed' or r.state == 'cancelled') then return 0 end
end
local now = tonumber(nowms())
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', string.format('%.0f', now))
redis.call('ZADD', KEYS[2], string.format('%.0f', now + tonumber(ARGV[2])), ARGV[1])
redis.call('PEXPIRE', KEYS[2], ARGV[2])
redis.call('PUBLISH', KEYS[3], 'cancel:' .. ARGV[1])
return 1`)

// CancelJob asks every process to cancel job id. A waiting job is dropped
// by the pop that would hand it out (record and outcome cancelled); a
// running one is announced to OnCancel callbacks, and ends only if its
// worker stops it. contract.ErrJobEnded when its record shows it ended. An
// unknown id is marked too (24 h): a later push of it under that id is
// dropped, unless EnqueueOnce pushes it (that clears the mark).
func (q *Queue) CancelJob(id string) error {
	if id == "" {
		return errEmptyJobID
	}
	if q.ctx.Err() != nil {
		return errQueueClosed
	}
	keys := []string{q.getJobsKey(), q.getCancelledKey(), q.getControlKey()}
	n, err := cancelJobScript.Run(q.ctx, q.client, keys, id, cancelTTL.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if n == 0 {
		return contract.ErrJobEnded
	}
	return nil
}

// AcknowledgeCancelled finishes a processing job that stopped because it
// was cancelled: as Reject (counted failed, kept under the failed
// retention, never requeued), but record and outcome state cancelled, with
// reason as the error.
func (q *Queue) AcknowledgeCancelled(ackID, reason string) bool {
	return q.finish(ackID, string(contract.JobCancelled), reason, nil)
}

// cancelHooks are a Queue's OnCancel callbacks and the cancel requests
// waiting for them, run by cancelLoop.
type cancelHooks struct {
	mu      sync.Mutex
	hooks   map[*func(string)]struct{}
	pending []string      // job ids not yet passed to the hooks, oldest first
	kick    chan struct{} // buffer 1
	loop    sync.Once     // starts cancelLoop on the first request with a hook
}

// OnCancel calls fn with the job ID of each cancel request on the queue
// (from any process, this one included), until stop. A call already taken
// for a request may still run once after stop returns. Calls come from one
// goroutine of this object, in order, off the pub/sub reader, so a slow fn
// delays only later calls. A request whose message was lost (a reconnect)
// is not repeated.
func (q *Queue) OnCancel(fn func(jobID string)) (stop func()) {
	h := &fn
	c := &q.cancelHooks
	c.mu.Lock()
	if c.hooks == nil {
		c.hooks = map[*func(string)]struct{}{}
	}
	c.hooks[h] = struct{}{}
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		delete(c.hooks, h)
		c.mu.Unlock()
	}
}

// cancelRequested queues job id for the OnCancel callbacks; called from the
// pub/sub reader. Without callbacks it is dropped.
func (q *Queue) cancelRequested(id string) {
	c := &q.cancelHooks
	c.mu.Lock()
	if id == "" || len(c.hooks) == 0 {
		c.mu.Unlock()
		return
	}
	c.pending = append(c.pending, id)
	c.mu.Unlock()

	c.loop.Do(func() { go q.cancelLoop() })
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// cancelLoop, one per Queue object from its first cancel request until
// Close, passes each pending id to the callbacks registered at that moment.
// A hook taken for an id may run once more after its stop returns.
func (q *Queue) cancelLoop() {
	c := &q.cancelHooks
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-c.kick:
		}

		for q.ctx.Err() == nil {
			c.mu.Lock()
			if len(c.pending) == 0 {
				c.mu.Unlock()
				break
			}
			id := c.pending[0]
			c.pending[0] = ""
			c.pending = c.pending[1:]
			fns := make([]func(string), 0, len(c.hooks))
			for h := range c.hooks {
				fns = append(fns, *h)
			}
			c.mu.Unlock()

			for _, fn := range fns {
				fn(id)
			}
		}
	}
}

// cancelFn defines cancelling(), dropped(v), budget and trimDropped() for
// the pop scripts (spec 6.14), after deadlineFn, envIDFn, asJSONFn and
// trimFn. k is the KEYS index of
// :cancelled, which :jobs, :completed, :failed and :attempts follow; a the
// ARGV index of the failed retention (most, age ms), which the outcome
// channel prefix follows.
//
// cancelling() reports whether any job is marked, so a pop without marks
// costs one EXISTS. dropped(v) drops item v if its envelope id has a mark
// that has not expired (an item without an id is never dropped; an expired
// mark is removed and counts as none): it clears the mark and the attempts
// count, writes a record with state cancelled (no started_at, no errors),
// indexed in :failed under the failed retention, publishes outcome
// cancelled, and returns true. It counts nothing in :stats. The item goes
// through asjson, as in finishScript.
//
// budget caps the drops of one script call at maxPop, so a backlog of
// marked items can't hold Redis in one long script: a pop whose budget ran
// out stops and returns what it has, and its caller pops again (see
// popGate, popOneGate). trimDropped() trims :failed once, after the drops.
func cancelFn(k, a string) string {
	return `local ck, ca = ` + k + `, ` + a + `
local budget = ` + strconv.Itoa(maxPop) + `
local function cancelling() return redis.call('EXISTS', KEYS[ck]) == 1 end
local function trimDropped()
	if budget < ` + strconv.Itoa(maxPop) + ` then
		trim(KEYS[ck + 3], KEYS[ck + 1], tonumber(ARGV[ca]), tonumber(ARGV[ca + 1]), tonumber(nowms()))
	end
end
local function dropped(v)
	local id = envid(v)
	if id == '' then return false end
	local exp = redis.call('ZSCORE', KEYS[ck], id)
	if not exp then return false end
	redis.call('ZREM', KEYS[ck], id)
	local now = tonumber(nowms())
	if tonumber(exp) <= now then return false end
	budget = budget - 1
	local attempts = 1 + (tonumber(redis.call('HGET', KEYS[ck + 4], id)) or 0)
	redis.call('HDEL', KEYS[ck + 4], id)
	local keep, age = tonumber(ARGV[ca]), tonumber(ARGV[ca + 1])
	redis.call('ZREM', KEYS[ck + 2], id)
	if keep > 0 and age > 0 then
		redis.call('HSET', KEYS[ck + 1], id, '{"id":' .. cjson.encode(id) .. ',"state":"cancelled","item":' .. asjson(v) ..
			',"finished_at":' .. string.format('%.0f', now) .. ',"attempts":' .. string.format('%.0f', attempts) .. '}')
		redis.call('ZADD', KEYS[ck + 3], string.format('%.0f', now), id)
	else
		redis.call('HDEL', KEYS[ck + 1], id)
	end
	redis.call('PUBLISH', ARGV[ca + 2] .. id, '{"id":' .. cjson.encode(id) .. ',"state":"cancelled","error":"cancelled"}')
	return true
end
`
}
