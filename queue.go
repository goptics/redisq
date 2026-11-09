package redisq

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

// Queue is a FIFO queue shared across processes: acknowledged pops,
// heartbeats, wakes for idle consumers (Notification) and job outcomes
// (WatchOutcome).
type Queue struct {
	*Notification // wakes; built in start, on this object's subscription
	client        *redis.Client
	queueKey      string
	ctx           context.Context
	cancel        context.CancelFunc
	// Config is atomic so no lock is held across a network call: Redis applies
	// each command, pipeline and script on its own.
	expiration            atomic.Int64                // time.Duration
	visibilityTimeout     atomic.Int64                // time.Duration
	ackPrefix             string                      // ack IDs are <ackPrefix>:<reservation>:<index>, unique per Queue object
	ackSeq                atomic.Uint64               // last reservation number handed out
	retryPop              atomic.Pointer[reservation] // ack IDs of a pop that failed with an error, reused by the next pop
	popLen                atomic.Int64                // items the next batch pop expects (see dequeueBatch); maxPop until the first
	popRun                atomic.Int64                // items batch pops took since the queue was last seen empty
	isPriority            bool                        // a zset queue, with :prio entries to clear on ack; set before any goroutine starts
	dedupWindow           atomic.Int64                // time.Duration EnqueueOnce remembers an id for
	heartbeatTimeout      atomic.Int64                // time.Duration (see SetHeartbeatTimeout)
	beating               atomic.Bool                 // a pop set this queue's heartbeat since a lapse was last logged
	tickScript            *redis.Script               // the sweep for this storage (see tickBody)
	releaseScript         *redis.Script               // what Close runs to requeue this queue's items (see releaseBody)
	storageKeys           []string                    // keys both take after the deadlines: :seq and :prio for a zset
	presence              *redis.PubSub               // this object's wake channel subscription (see start)
	keepCompleted         atomic.Int64                // most completed Job Records kept (see SetCompletedRetention)
	keepCompletedAge      atomic.Int64                // time.Duration a completed Job Record is kept for
	keepFailed            atomic.Int64                // most failed Job Records kept (see SetFailedRetention)
	keepFailedAge         atomic.Int64                // time.Duration a failed Job Record is kept for
	paused                atomic.Bool                 // the queue pause as this object last saw it (see pause.go)
	pauseHooks            pauseHooks                  // OnPauseChange callbacks
	pausedKey, controlKey string                      // <key>:paused and <key>:control, built once
	cancelHooks           hookSet[string]             // OnCancel callbacks (see cancel.go)
	rawKeys               []string                    // what Dequeue's scripts take
	closeOnce             sync.Once
	outcomes              outcomeWatchers // WatchOutcome watchers (see outcome.go)
	scope                 string          // see ScopeID; "" for a queue made by newQueueBase alone
}

func newQueue(c *redis.Client, queueKey string, scope string) *Queue {
	q := newQueueBase(c, queueKey)
	q.scope = scope
	q.tickScript, q.releaseScript = listTick, listRelease
	q.start()
	return q
}

// start subscribes this queue object to its wake channel and the queue's
// control channel (pause and resume, see pause.go), starts its Notification
// on that subscription, and sweeps once now, which requeues the items past
// their deadline or held by a dead or gone owner, and then every second.
// The subscription says the process is alive: Redis drops it within
// milliseconds of the process exiting, and other processes' sweeps then
// requeue its items (see tickBody). Outcome channels (WatchOutcome) join it
// too. Sweeping at once lets a restarted process start the grace of one
// that crashed straight away.
func (q *Queue) start() {
	q.presence = q.client.Subscribe(q.ctx, q.queueKey+":wake:"+q.ackPrefix, q.getControlKey())
	q.Notification = newNotification(q.client, q.presence, q.queueKey, q.lenCmd(), q.ackPrefix, q.handle)
	q.tick()
	go q.sweep()
	q.Notification.Start()
}

// handle routes a message of the queue's subscription that is not a wake:
// control (see Queue.control) and job outcomes (see outcome.go). It reports
// whether it took the message.
func (q *Queue) handle(msg *redis.Message) bool {
	return q.control(msg) || q.outcome(msg)
}

func newQueueBase(c *redis.Client, queueKey string) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		client:    c,
		queueKey:  queueKey,
		ctx:       ctx,
		cancel:    cancel,
		ackPrefix: newID(),
	}
	q.pauseHooks.kick = make(chan struct{}, 1)
	q.pausedKey, q.controlKey = queueKey+":paused", queueKey+":control"
	q.rawKeys = []string{queueKey, q.pausedKey, q.getCancelledKey(), q.getJobsKey(), q.getCompletedKey(), q.getFailedKey(), q.getAttemptsKey()}
	q.visibilityTimeout.Store(int64(5 * time.Minute))
	q.dedupWindow.Store(int64(5 * time.Minute))
	q.heartbeatTimeout.Store(int64(30 * time.Second))
	q.popLen.Store(maxPop)
	q.SetCompletedRetention(1000, 24*time.Hour)
	q.SetFailedRetention(10000, 7*24*time.Hour)
	return q
}

// newID returns an ID unique across processes: 128 random bits from
// crypto/rand. cuid.New, used before, seeded math/rand with the start second,
// so containers with the same pid and host started together could share IDs,
// and a pop's re-send guard could hand one consumer the items another holds.
func newID() string {
	return crand.Text()
}

func (q *Queue) getExpiration() time.Duration {
	return time.Duration(q.expiration.Load())
}

func (q *Queue) getVisibilityTimeout() time.Duration {
	return time.Duration(q.visibilityTimeout.Load())
}

// SetExpiration sets the expiration time for the Queue: each push of this
// object renews it. Workflow pushes (Submit, promotions, Purge releases)
// don't, so a workflow refuses a task on a queue object with an
// expiration: an expired task would leave its workflow running forever.
func (q *Queue) SetExpiration(expiration time.Duration) {
	q.expiration.Store(int64(expiration))
}

// SetVisibilityTimeout sets the visibility timeout for jobs
// This controls how long a job remains invisible in the nacked queue before being candidates for requeuing
func (q *Queue) SetVisibilityTimeout(d time.Duration) {
	q.visibilityTimeout.Store(int64(d))
}

// minHeartbeat is the shortest heartbeat timeout SetHeartbeatTimeout takes:
// a few sweeps, so one slow call doesn't make a live queue look dead.
const minHeartbeat = 5 * time.Second

// SetHeartbeatTimeout sets how long this queue may go without reaching Redis
// before other processes on the key take it for dead and requeue the items it
// dequeued and has not acknowledged (default 30s, at least 5s). Each pop that
// hands out items and each sweep, once a second, renew the heartbeat, so it
// runs out only when the process dies, freezes or loses Redis. Unlike the
// visibility timeout it doesn't limit how long a job may run: a job that takes
// longer than the visibility timeout still comes back, heartbeat or not.
//
// Set it above the longest freeze or network cut you want to ride out: a job
// still running when its queue's heartbeat runs out runs again elsewhere, and
// its later Acknowledge still reports success. A stall of Redis itself, such
// as a failover, doesn't count. A queue whose process exits is taken for dead
// sooner, about 3s after Redis drops its pub/sub connection.
// 0 or less turns both off, so this queue's items wait for the visibility
// timeout.
func (q *Queue) SetHeartbeatTimeout(d time.Duration) {
	if d > 0 {
		d = max(d, minHeartbeat)
	}
	q.heartbeatTimeout.Store(int64(d))
}

// hbt is the heartbeat timeout in seconds, as the scripts take it (0: none).
func (q *Queue) hbt() float64 {
	return max(0, time.Duration(q.heartbeatTimeout.Load()).Seconds())
}

// Dequeue removes and returns an item from the queue without acknowledgment
// For reliable processing with acknowledgment, use DequeueWithAck instead
// A consumer that finds the queue empty joins the idle set.
func (q *Queue) Dequeue() (any, bool) {
	return q.rawPop(listRawPop)
}

// listRawPop and zsetRawPop are Dequeue's pop without acknowledgment: 0 while
// KEYS[2], :paused, exists, else the item (a zset's without its sequence
// prefix), or false when the queue is empty. Cancelled items are dropped
// (see cancelFn); -1 when the drop budget ran out first (pop again). KEYS[3..7]: :cancelled, :jobs, :completed, :failed,
// :attempts. ARGV: failed retention (most, age ms), outcome channel prefix.
var (
	listRawPop = rawPopScript(listPop1)
	zsetRawPop = rawPopScript(zsetPop1)
)

func rawPopScript(pop1 string) *redis.Script {
	return redis.NewScript(deadlineFn + cancelFn("3", "1") +
		"if redis.call('EXISTS', KEYS[2]) == 1 then return 0 end\n" + pop1 + popOneGate +
		"if not v and budget <= 0 then return -1 end\nreturn v or false")
}

// spent reports a pop reply of -1: the script's drop budget ran out before
// it found an item to hand out (see cancelFn), so the caller pops again.
func spent(reply any) bool {
	n, ok := reply.(int64)
	return ok && n < 0
}

// rawPop is Dequeue with pop: the item as stored, as []byte. A consumer
// whose pop finds the queue empty joins the idle set; a paused miss stays
// out (spec 6.2).
func (q *Queue) rawPop(pop *redis.Script) (any, bool) {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot dequeue")
		return nil, false
	}
	g := q.pauseHooks.gen.Load()
	reply, err := pop.Run(q.ctx, q.client, q.rawKeys, q.cancelArgs()...).Result()
	for err == nil && spent(reply) {
		reply, err = pop.Run(q.ctx, q.client, q.rawKeys, q.cancelArgs()...).Result()
	}
	if err == redis.Nil {
		if !q.Paused() {
			q.markIdle()
		}
		return nil, false
	}

	if err != nil {
		log.Printf("Error dequeuing: %v", err)
		return nil, false
	}

	v, ok := reply.(string)
	if !ok { // paused
		q.refused(g)
		return nil, false
	}

	q.idle.Store(false)
	return []byte(v), true
}

func (q *Queue) toBytes(item any) ([]byte, error) {
	var data []byte
	switch v := item.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return nil, fmt.Errorf("unsupported type: %T", v)
	}
	return data, nil
}

// cjsonMaxDepth is the nesting Redis's Lua cjson decodes at most.
const cjsonMaxDepth = 1000

// luaDecodable reports false for JSON that Go's encoding/json reads but
// Redis's Lua cjson can't: nested deeper than cjsonMaxDepth, or with a lone
// UTF-16 surrogate escape (\ud800 not followed by a low one, or a low one
// alone). The pop's cancel gate can't read such an item's id, so a cancel
// would miss it while varmq still runs it. One pass, no allocation; only a
// suspect item is checked with json.Valid, so text that isn't JSON passes.
func luaDecodable(b []byte) bool {
	depth, str := 0, false
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case str && c == '"':
			str = false
		case str && c == '\\':
			i++
			if i >= len(b) || b[i] != 'u' {
				continue
			}
			r := hex4(b, i+1)
			if r < 0xD800 || r > 0xDFFF {
				i += 4
				continue
			}
			if r > 0xDBFF || i+10 >= len(b) || b[i+5] != '\\' || b[i+6] != 'u' {
				return !json.Valid(b)
			}
			if lo := hex4(b, i+7); lo < 0xDC00 || lo > 0xDFFF {
				return !json.Valid(b)
			}
			i += 10
		case str:
		case c == '"':
			str = true
		case c == '{' || c == '[':
			if depth++; depth > cjsonMaxDepth {
				return !json.Valid(b)
			}
		case c == '}' || c == ']':
			depth--
		}
	}
	return true
}

// hex4 is the value of the 4 hex digits at b[i:], or -1.
func hex4(b []byte, i int) int {
	var d [2]byte
	if i+4 > len(b) {
		return -1
	}
	if _, err := hex.Decode(d[:], b[i:i+4]); err != nil {
		return -1
	}
	return int(d[0])<<8 | int(d[1])
}

// Enqueue pushes item and wakes idle consumers, up to one per waiting item,
// in one round trip. It refuses (false) JSON that Redis's Lua cjson can't
// decode though Go can: nested deeper than 1000, or with a lone UTF-16
// surrogate escape such as \ud800 (see luaDecodable). One rule for every
// item, with an envelope id or not.
func (q *Queue) Enqueue(item any) bool {
	return q.enqueueWith(listPushWake, q.pushKeys(), item, q.wakePrefix)
}

// pushKeys are the keys listPushWake and listPushWakeOnce take before the
// id's key.
func (q *Queue) pushKeys() []string {
	return []string{q.queueKey, q.getIdleKey(), q.getStatsKey()}
}

// SetDedupWindow sets how long EnqueueOnce remembers an id: a push of the
// same id within it stores nothing. Keep it above the client's re-send window,
// or a late re-send can store the item again: with the client timeouts New sets
// up to 7 sends of a script (3 re-sends as EVALSHA, then up to 4 as EVAL after
// a NOSCRIPT), each after a read timeout, a backoff, a pool wait, a dial and
// a connection's setup, about 4 minutes at worst. 0 or less turns the dedupe
// off: EnqueueOnce always pushes, and still drops the id's old record and
// cancel mark. Each id costs about 100 bytes of Redis memory for the window.
func (q *Queue) SetDedupWindow(d time.Duration) {
	q.dedupWindow.Store(int64(d))
}

// once wraps a push script body so it stores its item once per id:
// KEYS[#KEYS] is the id's key and ARGV[#ARGV] the dedupe window in ms. A key
// already there means a push of the id (an earlier call, or a copy of this
// one that go-redis re-sent) ran within the window, and nothing is pushed.
// A window of 0 skips the check and the key: every call pushes.
// The key is set only after the push: Redis keeps a script's earlier writes
// when a later command fails, so a push that fails (a key of the wrong type)
// leaves the id free for a retry instead of silently taken.
//
// A push also drops the id's (ARGV[#ARGV - 1]) old Job Record: KEYS[#KEYS -
// 3] :jobs, KEYS[#KEYS - 2] :completed, KEYS[#KEYS - 1] :failed. Else an id
// reused after the window has WatchOutcome's catch-up report the previous
// run's outcome and result (spec 6.13). And its cancel mark, KEYS[#KEYS - 4]
// :cancelled, so the new job isn't dropped (spec 6.14). A dedupe hit changes
// nothing.
func once(body string) string {
	return `local win = tonumber(ARGV[#ARGV])
if win > 0 and redis.call('EXISTS', KEYS[#KEYS]) == 1 then return 0 end
local r = (function()
` + body + `
end)()
if win > 0 then redis.call('SET', KEYS[#KEYS], '1', 'PX', ARGV[#ARGV]) end
redis.call('HDEL', KEYS[#KEYS - 3], ARGV[#ARGV - 1])
redis.call('ZREM', KEYS[#KEYS - 2], ARGV[#ARGV - 1])
redis.call('ZREM', KEYS[#KEYS - 1], ARGV[#ARGV - 1])
redis.call('ZREM', KEYS[#KEYS - 4], ARGV[#ARGV - 1])
return r`
}

// listPushWakeBody is Enqueue's push, with ARGV[1] the item, ARGV[2] the
// queue expiration in ms (0: none) and ARGV[3] the wake channel prefix. It
// wakes idle consumers of KEYS[2], up to one per waiting item, so a backlog
// wakes them all, and counts the job as submitted in KEYS[3], :stats; a
// dedupe repeat (see once) never gets here, so it isn't counted and wakes
// no one.
const listPushWakeBody = `local n = redis.call('RPUSH', KEYS[1], ARGV[1])
if tonumber(ARGV[2]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
redis.call('HINCRBY', KEYS[3], 'submitted', 1)
return wake(KEYS[2], ARGV[3], n)`

var (
	listPushWake     = redis.NewScript(wakeFn + listPushWakeBody)
	listPushWakeOnce = redis.NewScript(wakeFn + once(listPushWakeBody))
)

// EnqueueOnce is Enqueue for a job with an id: it stores item unless an item
// with the same id was pushed to this queue within the dedupe window (see
// SetDedupWindow), and returns true in both cases. A retried call, or
// go-redis's re-send of a push whose reply was lost, then stores the item
// once. The id is remembered for the whole window, even if its item is
// handled, removed or purged sooner. An empty id is a plain Enqueue. A
// repeat wakes no one. With no window it always pushes, but still drops the
// id's old record and cancel mark (see once).
func (q *Queue) EnqueueOnce(item any, id string) bool {
	if id == "" {
		return q.Enqueue(item)
	}
	return q.enqueueOnceWith(listPushWakeOnce, q.pushKeys(), item, id, q.wakePrefix)
}

// enqueueOnceWith is enqueueWith for a script built with once: it adds the
// cancel mark, record and id's keys to keys, and id and the dedupe window
// to the arguments. A window under 1ms counts as 1ms; none as 0.
func (q *Queue) enqueueOnceWith(script *redis.Script, keys []string, item any, id string, extra ...any) bool {
	window := int64(0)
	if d := time.Duration(q.dedupWindow.Load()); d > 0 {
		window = max(1, d.Milliseconds())
	}
	keys = append(keys, q.getCancelledKey(), q.getJobsKey(), q.getCompletedKey(), q.getFailedKey(), q.queueKey+":dedup:"+id)
	return q.enqueueWith(script, keys, item, append(extra, id, window)...)
}

// enqueueWith runs script to push item, with ARGV[1] the item bytes, ARGV[2]
// the queue expiration in milliseconds (0: none) and then extra.
func (q *Queue) enqueueWith(script *redis.Script, keys []string, item any, extra ...any) bool {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot enqueue")
		return false
	}

	data, err := q.toBytes(item)
	if err != nil {
		log.Printf("Error converting item to bytes: %v", err)
		return false
	}
	if !luaDecodable(data) {
		log.Println("Error enqueueing item: JSON Redis Lua can't decode (nested over 1000, or a lone surrogate escape)")
		return false
	}

	args := append([]any{data, q.getExpiration().Milliseconds()}, extra...)
	if err := script.Run(q.ctx, q.client, keys, args...).Err(); err != nil {
		log.Printf("Error enqueueing item: %v", err)
		return false
	}

	return true
}

// Len returns the number of items in the queue; a consumer that finds it
// empty joins the idle set.
func (q *Queue) Len() int {
	return q.Notification.len()
}

func (q *Queue) Values() []any {
	results, err := q.client.LRange(q.ctx, q.queueKey, 0, -1).Result()
	if err != nil {
		return []any{}
	}

	values := make([]any, 0, len(results))
	for _, result := range results {
		// Just convert the string to bytes without base64 decoding
		values = append(values, []byte(result))
	}

	return values
}

// prepareAck stores item ARGV[2] as pending under ack ID ARGV[1], with a
// visibility deadline ARGV[3] seconds from now (see deadlineFn), and its
// start time in KEYS[3], :started.
var prepareAck = redis.NewScript(deadlineFn + `redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
local dl = deadline(ARGV[3])
if dl > 0 then redis.call('ZADD', KEYS[2], dl, ARGV[1]) end
redis.call('HSET', KEYS[3], ARGV[1], nowms())
return 1`)

// PrepareForFutureAck adds an item to the pending list for acknowledgment tracking
// Returns an error if the operation fails
func (q *Queue) PrepareForFutureAck(ackID string, item any) error {
	select {
	case <-q.ctx.Done():
		return fmt.Errorf("queue closed, cannot prepare for acknowledgment")
	default:
		keys := []string{q.getNackedItemKey(), q.getTimeoutKey(), q.getStartedKey()}
		if err := prepareAck.Run(q.ctx, q.client, keys, ackID, item, q.vt()).Err(); err != nil {
			return fmt.Errorf("error storing item in pending list: %v", err)
		}

		return nil
	}
}

// Acknowledge marks a processing job completed: it leaves processing,
// counts as completed in :stats, and its Job Record is kept as retention
// allows (see SetCompletedRetention), in one atomic step. An ack ID that is
// no longer processing (acknowledged, rejected, requeued or purged) is a
// success that does nothing. A job with an envelope id has its outcome
// published to watchers (see WatchOutcome).
func (q *Queue) Acknowledge(ackID string) bool {
	return q.finish(ackID, "completed", "", nil)
}

// AcknowledgeResult is Acknowledge that keeps result, the result worker's
// JSON, in the job's record ("result") and outcome. Invalid JSON returns
// false and changes nothing. No size limit here (varmq caps it).
func (q *Queue) AcknowledgeResult(ackID string, result []byte) bool {
	if !json.Valid(result) {
		return false
	}
	return q.finish(ackID, "completed", "", result)
}

// Reject marks a processing job failed for good, with reason as its last
// error: like Acknowledge, but counted as failed and kept under the failed
// retention (see SetFailedRetention). The job is not requeued.
func (q *Queue) Reject(ackID string, reason string) bool {
	return q.finish(ackID, "failed", reason, nil)
}

// finish ends ackID's job in state (completed, failed or cancelled). A
// cancelled job is indexed, kept and counted as failed (spec 6.14: as Reject).
func (q *Queue) finish(ackID, state, reason string, result []byte) bool {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot acknowledge or reject")
		return false
	}

	keys, args := q.finishArgs(ackID, state, reason, result)
	r, err := finishScript.Run(q.ctx, q.client, keys, args...).Int()
	if err != nil {
		log.Printf("Error finishing item as %s: %v", state, err)
		return false
	}
	return r >= 0 // -1: result is not JSON
}

// finishArgs are finishScript's KEYS and ARGV (settleFn's too) for ending
// ackID's job in state.
func (q *Queue) finishArgs(ackID, state, reason string, result []byte) ([]string, []any) {
	this, other := q.getCompletedKey(), q.getFailedKey()
	keep, age := q.keepCompleted.Load(), q.keepCompletedAge.Load()
	if state != "completed" {
		this, other = other, this
		keep, age = q.keepFailed.Load(), q.keepFailedAge.Load()
	}
	keys := []string{q.getNackedItemKey(), q.getTimeoutKey(), q.getStartedKey(), q.getAttemptsKey(), q.getStatsKey(), q.getJobsKey(), this, other, q.getPriorityKey(), q.getCancelledKey()}
	return keys, []any{ackID, state, reason, keep, time.Duration(age).Milliseconds(), result, q.donePrefix()}
}

// SetCompletedRetention sets how many completed Job Records the queue keeps
// (default 1000) and for how long (default 24h). Either at 0 or less keeps
// none: no record is written and the next trim drops the ones there.
// Every instance of a queue should set the same values; each trim uses
// those of the instance that runs it. The :stats counters count every job
// whatever the retention.
func (q *Queue) SetCompletedRetention(most int, age time.Duration) {
	q.keepCompleted.Store(int64(max(0, most)))
	q.keepCompletedAge.Store(int64(max(0, age)))
}

// SetFailedRetention is SetCompletedRetention for failed (rejected) jobs
// (default 10000 for 7 days).
func (q *Queue) SetFailedRetention(most int, age time.Duration) {
	q.keepFailed.Store(int64(max(0, most)))
	q.keepFailedAge.Store(int64(max(0, age)))
}

// envIDFn defines envid(v): the envelope id of item v, or empty when v is not
// a JSON object with a string id, and the decoded envelope (nil when v is
// not JSON). Items are only decoded, never re-encoded: cjson.encode rounds
// numbers to 14 digits.
//
// It also defines wftag(e, id) (spec 3): whether envelope e, with id, holds
// workflow fields a Submit could write: wf a workflow ID, task a task name,
// undo absent, null (Go reads it as false) or a boolean, and id
// wf:<wf>:<task> (":undo" appended for undo true). Mirrors varmq's taskTag.matches; anything else is a plain job
// and no wf: key is built from it. Go's encoding/json matches keys without
// case (and folds "ſ" to s, "K" to k), cjson does not: an envelope with
// any such variant of id, wf, task or undo (wffold) reads differently in
// varmq, so it is never a workflow item here and a cancel mark drops it.
const envIDFn = `local function envid(v)
	local ok, e = pcall(cjson.decode, v)
	if ok and type(e) == 'table' and type(e.id) == 'string' then return e.id, e end
	return '', (ok and e or nil)
end
local function wfname(s, n)
	return type(s) == 'string' and #s >= 1 and #s <= n and string.find(s, '^[A-Za-z0-9_.%-]+$') ~= nil
end
local function wffold(k)
	if type(k) ~= 'string' then return nil end
	return string.lower((string.gsub(string.gsub(k, '\197\191', 's'), '\226\132\170', 'k')))
end
local function wftag(e, id)
	if type(e) ~= 'table' or e.wf == nil then return false end
	for k in pairs(e) do
		local f = wffold(k)
		if (f == 'id' or f == 'wf' or f == 'task' or f == 'undo') and k ~= f then return false end
	end
	if e.undo ~= nil and e.undo ~= cjson.null and type(e.undo) ~= 'boolean' then return false end
	if not wfname(e.wf, 128) or e.wf == 'index' or not wfname(e.task, 64) then return false end
	return id == 'wf:' .. e.wf .. ':' .. e.task .. (e.undo == true and ':undo' or '')
end
`

// asJSONFn defines asjson(v): item v as it goes into a record, v itself
// when it is JSON, else v as a JSON string. cjson.decode takes nan, inf and
// hex numbers, so a bare number counts as JSON only when it looks like one.
const asJSONFn = `local function asjson(v)
	local ok, e = pcall(cjson.decode, v)
	if not ok or (type(e) == 'number' and not string.match(v, '^%s*-?%d[%d.eE+-]*%s*$')) then return cjson.encode(v) end
	return v
end
`

// trimFn defines trim(z, jobs, keep, age, now) for the finish and sweep
// trims: it drops the members of Job State ZSET z older than age ms or
// outside the newest keep, all of them when either is 0, and their records
// in HASH jobs. Members are collected before they go, so their records go
// too. A trim takes up to 1000 of each; the next one takes the rest.
const trimFn = `local function trim(z, jobs, keep, age, now)
	local gone
	if keep <= 0 or age <= 0 then
		gone = redis.call('ZRANGE', z, 0, 999)
	else
		gone = redis.call('ZRANGEBYSCORE', z, '-inf', string.format('%.0f', now - age), 'LIMIT', 0, 1000)
		local over = redis.call('ZCARD', z) - keep
		if over > 0 then
			for _, m in ipairs(redis.call('ZRANGE', z, 0, math.min(over, 1000) - 1)) do gone[#gone + 1] = m end
		end
	end
	if #gone > 0 then
		redis.call('ZREM', z, unpack(gone))
		redis.call('HDEL', jobs, unpack(gone))
	end
end
`

// finishScript is Acknowledge, Reject and AcknowledgeCancelled
// (spec/redis-storage.md 6.3, 6.4, 6.14).
//
// KEYS: nacked, timeouts, started, attempts, stats, jobs, the state's ZSET,
// the other state's ZSET, prio, cancelled. ARGV: ack ID, state (completed,
// failed or cancelled), reason, most records kept, longest age (ms), result
// JSON ("" for none), outcome channel prefix (<base>:done:). :stats counts
// completed, else failed.
//
// Every finish drops the Job ID's cancel mark: the job ended.
//
// A result that cjson can't decode returns -1 before any write (Go's
// json.Valid passes some it rejects, e.g. deep nesting). A completed
// record gets "result", spliced raw: a cjson round trip turns big integers
// into doubles. A job with an envelope id has its outcome (contract
// JobOutcome) published on <base>:done:<id> after the record step, kept or
// not (spec 6.13).
//
// The Job ID is the envelope id, else the ack ID. Record and outcome come
// from recordFn. The body is settleFn's settlejob(), shared with FinishTask.
var finishScript = redis.NewScript(settleFn + `return settlejob()`)

// settleFn defines settlejob(), finishScript's body over KEYS[1..10] and
// ARGV[1..7] (see finishScript), so FinishTask settles a workflow item with
// the same code. It brings deadlineFn, envIDFn, asJSONFn, trimFn, recordFn.
const settleFn = deadlineFn + envIDFn + asJSONFn + trimFn + recordFn + `local function settlejob()
if ARGV[6] ~= '' and not pcall(cjson.decode, ARGV[6]) then return -1 end
local item = redis.call('HGET', KEYS[1], ARGV[1])
if not item then return 0 end
local started = tonumber(redis.call('HGET', KEYS[3], ARGV[1]))
redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
redis.call('HDEL', KEYS[9], ARGV[1])
local env = envid(item)
local id, attempts = env, 1
if id ~= '' then
	attempts = 1 + (tonumber(redis.call('HGET', KEYS[4], id)) or 0)
	redis.call('HDEL', KEYS[4], id)
else
	id = ARGV[1]
end
redis.call('ZREM', KEYS[10], id)
local done = ARGV[2] == 'completed'
redis.call('HINCRBY', KEYS[5], done and 'completed' or 'failed', 1)
local now = tonumber(nowms())
local keep, age = tonumber(ARGV[4]), tonumber(ARGV[5])
local err = not done and ARGV[3] or nil
local res = (done and ARGV[6] ~= '') and (',"result":' .. ARGV[6]) or ''
redis.call('ZREM', KEYS[8], id)
if keep > 0 and age > 0 then
	redis.call('HSET', KEYS[6], id, record(id, ARGV[2], item, started, now, attempts, err, res))
	redis.call('ZADD', KEYS[7], string.format('%.0f', now), id)
else
	redis.call('HDEL', KEYS[6], id)
end
trim(KEYS[7], KEYS[6], keep, age, now)
if env ~= '' then redis.call('PUBLISH', ARGV[7] .. env, outcome(env, ARGV[2], err, res)) end
return 1
end
`

// trimScript trims both Job States (see trimFn), so an idle queue's records
// still age out. KEYS: jobs, completed, failed. ARGV: most completed kept,
// their longest age (ms), most failed kept, their longest age (ms).
var trimScript = redis.NewScript(deadlineFn + trimFn + `local now = tonumber(nowms())
trim(KEYS[2], KEYS[1], tonumber(ARGV[1]), tonumber(ARGV[2]), now)
trim(KEYS[3], KEYS[1], tonumber(ARGV[3]), tonumber(ARGV[4]), now)
return 1`)

// trim runs trimScript with this queue's retention.
func (q *Queue) trim() {
	keys := []string{q.getJobsKey(), q.getCompletedKey(), q.getFailedKey()}
	err := trimScript.Run(q.ctx, q.client, keys,
		q.keepCompleted.Load(), time.Duration(q.keepCompletedAge.Load()).Milliseconds(),
		q.keepFailed.Load(), time.Duration(q.keepFailedAge.Load()).Milliseconds()).Err()
	if err != nil && q.ctx.Err() == nil {
		log.Printf("Error trimming job records: %v", err)
	}
}

// maxPop caps the items one pop call takes. A script unpacks 2*maxPop values
// in one call, which must stay under Lua's 8000-value limit.
const maxPop = 1000

// popHead and popTail wrap the storage-specific part of the ack pop scripts,
// which leaves the popped items, as stored, in `items`.
//
// KEYS: queue, pending hash, deadlines, idle set [, priorities], cancelled,
// jobs, completed, failed, attempts, paused, start times, heartbeats (see
// popKeys).
// ARGV: want, visibility timeout (seconds; 0: none), ack ID
// prefix, reservation (20-digit start), consumer ID (” if the caller is not
// a distributed consumer), the caller's free slots, failed retention (most,
// age ms) and outcome channel prefix (see cancelFn), heartbeat timeout
// (seconds; 0: none), the ack ID prefix again (see beatFn).
//
// Item i is pending under "<prefix>:<reservation>:<i as 4 digits>". IDs are
// built from strings, so no number goes through Lua arithmetic, and they sort
// in pop order. Redis runs each script atomically, so an item is always in
// the queue or in the pending hash.
//
// go-redis re-sends a command whose reply was lost (a read timeout or a
// dropped connection) with the same ARGV, so a script can run twice for one
// call, and a copy can even arrive after the caller acked some of the items.
// If any of the call's IDs is still pending, an earlier run popped: popHead
// pops nothing, so no live pending entry is overwritten, and hands back the
// leading run of those items with their deadlines raised to this run's. A
// batch shares one deadline, so a requeue moves all of it or none.
//
// A call that leaves the queue empty with fewer items than the caller's free
// slots makes a distributed consumer join the idle set, in the same step, so
// an enqueue right after it wakes it. A hand-back counts too: a reused
// reservation can be smaller than the call.
var popHead = deadlineFn + beatFn + cancelFn("#KEYS - 7", "#ARGV - 4") + `local want, pre, resv = tonumber(ARGV[1]), ARGV[3], ARGV[4]
local dl = deadline(ARGV[2])
local function reply(items)
	local len = redis.call(__LEN__, KEYS[1])
	if ARGV[5] ~= '' and len == 0 and #items < tonumber(ARGV[6]) then redis.call('SADD', KEYS[4], ARGV[5]) end
	return {len, items}
end
local ids = {}
for i = 1, want do ids[i] = pre .. ':' .. resv .. ':' .. string.format('%04d', i) end
local held = redis.call('HMGET', KEYS[2], unpack(ids))
for i = 1, want do
	if held[i] then
		local items = {}
		for j = 1, want do
			if not held[j] then break end
			items[j] = held[j]
			if dl > 0 then
				local d = redis.call('ZSCORE', KEYS[3], ids[j])
				if not d or tonumber(d) < dl then redis.call('ZADD', KEYS[3], dl, ids[j]) end
			end
		end
		if #items > 0 then beat() end
		return reply(items)
	end
end
local items = {}
`

const popTail = `
local n = #items
if n > 0 then
	local h, z = {}, {}
	for i = 1, n do
		h[2*i-1] = ids[i]; h[2*i] = items[i]
		z[2*i-1] = dl; z[2*i] = ids[i]
	end
	redis.call('HSET', KEYS[2], unpack(h))
	if dl > 0 then redis.call('ZADD', KEYS[3], unpack(z)) end
	local now = nowms()
	for i = 1, n do h[2*i] = now end
	redis.call('HSET', KEYS[#KEYS - 1], unpack(h))
	beat()
end
return reply(items)`

// popScript builds an ack pop script for storage whose length command is
// lenCmd: take(k) pops up to k items and returns them, as stored, with
// their scores (see listTake), popGate drops the cancelled ones, and after
// runs on the items handed out.
func popScript(lenCmd, take, after string) *redis.Script {
	return redis.NewScript(strings.ReplaceAll(popHead+pausedBatch+take+popGate+after+popTail, "__LEN__", "'"+lenCmd+"'"))
}

// popGate pops want items into items (scores in sc). With cancel marks
// (spec 6.14) it drops the marked items (see cancelFn) and pops more in
// their place, until it has want, the queue is empty or the drop budget is
// spent. It never takes more than the budget left, so every item it takes
// is handed out or dropped. A spent budget leaves items in the queue, so
// reply() reports them and adds no idle consumer: the caller pops again.
// Without marks it costs one EXISTS.
const popGate = `local sc
items, sc = take(want)
if #items > 0 and cancelling() then
	local all, alls = items, sc
	items, sc = {}, {}
	while #all > 0 do
		for i, v in ipairs(all) do
			if not dropped(v) then items[#items + 1] = v; sc[#items] = alls[i] end
		end
		if #items == want or budget <= 0 then break end
		all, alls = take(math.min(want - #items, budget))
	end
	trimDropped()
end
`

const (
	// listTake pops from the head of a list. LRANGE+LTRIM rather than LPOP
	// with a count, which needs Redis 6.2. A list has no scores.
	listTake = `local function take(k)
	local r = redis.call('LRANGE', KEYS[1], 0, k - 1)
	if #r > 0 then redis.call('LTRIM', KEYS[1], #r, -1) end
	return r, {}
end
`
	// zsetTake pops the lowest scores and returns the bare items (without
	// the sequence prefix).
	zsetTake = bareFn + `local function take(k)
	local r, it, s = redis.call('ZPOPMIN', KEYS[1], k), {}, {}
	for i = 1, #r / 2 do
		it[i] = bare(r[2*i-1]); s[i] = r[2*i]
	end
	return it, s
end
`
	// zsetKeepPrio keeps each handed-out item's score in KEYS[5] so a
	// requeue restores its priority.
	zsetKeepPrio = `if #items > 0 then
	local p = {}
	for i = 1, #items do p[2*i-1] = ids[i]; p[2*i] = sc[i] end
	redis.call('HSET', KEYS[5], unpack(p))
end
`
)

var (
	// dequeueBatch sends every one-item call to the lean script, so want is
	// at least 2 here.
	listAckPop = popScript("LLEN", listTake, "")
	zsetAckPop = popScript("ZCARD", zsetTake, zsetKeepPrio)
)

// popOneHead and popOneTail wrap the lean single-item pop that DequeueWithAckId
// runs, which leaves the popped item, as stored, in v (nil when the queue is
// empty; the script returns -1 when the drop budget ran out, see
// popOneGate). Same keys, ack IDs and rules as the batch scripts (see popHead),
// with none of the batch bookkeeping: per call it costs what a single pop did
// before batches.
//
// ARGV: visibility timeout (seconds; 0: none), ack ID, consumer ID (” if
// the caller is not a distributed consumer), failed retention (most, age
// ms), outcome channel prefix, heartbeat timeout, ack ID prefix.
var popOneHead = deadlineFn + beatFn + cancelFn("#KEYS - 7", "#ARGV - 4") + `local dl = deadline(ARGV[1])
local held = redis.call('HGET', KEYS[2], ARGV[2])
if held then
	if dl > 0 then
		local d = redis.call('ZSCORE', KEYS[3], ARGV[2])
		if not d or tonumber(d) < dl then redis.call('ZADD', KEYS[3], dl, ARGV[2]) end
	end
	beat()
	return held
end
`

const popOneTail = `
if not v then
	if budget <= 0 then return -1 end
	if ARGV[3] ~= '' then redis.call('SADD', KEYS[4], ARGV[3]) end
	return false
end
redis.call('HSET', KEYS[2], ARGV[2], v)
if dl > 0 then redis.call('ZADD', KEYS[3], dl, ARGV[2]) end
redis.call('HSET', KEYS[#KEYS - 1], ARGV[2], nowms())
beat()
return v`

// listPop1 and zsetPop1 define pop1(), which pops one item and returns it
// (false when empty) and its score (zset only); the zset one strips the
// sequence prefix.
const (
	listPop1 = `local function pop1() return redis.call('LPOP', KEYS[1]) end
`
	zsetPop1 = bareFn + `local function pop1()
	local r = redis.call('ZPOPMIN', KEYS[1])
	if not r[1] then return false end
	return bare(r[1]), r[2]
end
`
)

// popOneGate pops one item into v (false when empty), score sc. With cancel
// marks it drops marked items (see cancelFn) and pops the next, so a
// dropped head never leaves the consumer idle with work waiting. Once the
// drop budget is spent it stops with v false (budget 0): the single pop
// returns -1 instead of idling, the lean batch pop reports the items left.
const popOneGate = `local v, sc = pop1()
if v and cancelling() then
	while v and dropped(v) do
		if budget <= 0 then v = false; break end
		v, sc = pop1()
	end
	trimDropped()
end
`

// listPopOne and zsetPopOne pop one item into v; the zset one keeps its
// score in KEYS[5] (see zsetKeepPrio).
const (
	listPopOne = listPop1 + popOneGate
	zsetPopOne = zsetPop1 + popOneGate + `if v then redis.call('HSET', KEYS[5], ARGV[2], sc) end
`
)

var (
	listAckPopOne = redis.NewScript(popOneHead + pausedOne + listPopOne + popOneTail)
	zsetAckPopOne = redis.NewScript(popOneHead + pausedOne + zsetPopOne + popOneTail)
)

// popOneLenHead and popOneLenTail wrap the lean pop a batch call runs when it
// takes one item: popOneHead's guard and popOneTail's bookkeeping, with the
// batch scripts' reply ({len, items}) and idle rule (see popHead). It skips
// building and checking ack IDs for the slots it would not fill.
//
// ARGV: visibility timeout, ack ID, consumer ID, the caller's free slots,
// failed retention (most, age ms), outcome channel prefix, heartbeat
// timeout, ack ID prefix.
var popOneLenHead = `local function reply(items)
	local len = redis.call(__LEN__, KEYS[1])
	if ARGV[3] ~= '' and len == 0 and #items < tonumber(ARGV[4]) then redis.call('SADD', KEYS[4], ARGV[3]) end
	return {len, items}
end
` + strings.Replace(popOneHead, "return held", "return reply({held})", 1)

const popOneLenTail = `
if not v then return reply({}) end
redis.call('HSET', KEYS[2], ARGV[2], v)
if dl > 0 then redis.call('ZADD', KEYS[3], dl, ARGV[2]) end
redis.call('HSET', KEYS[#KEYS - 1], ARGV[2], nowms())
beat()
return reply({v})`

func popOneLenScript(lenCmd, body string) *redis.Script {
	return redis.NewScript(strings.ReplaceAll(popOneLenHead+pausedBatch+body+popOneLenTail, "__LEN__", "'"+lenCmd+"'"))
}

var (
	listAckPopOneLen = popOneLenScript("LLEN", listPopOne)
	zsetAckPopOneLen = popOneLenScript("ZCARD", zsetPopOne)
)

// reservation is the ack IDs <prefix>:<start>:0001..<start>:<want> of one pop
// call; start is unique per Queue object.
type reservation struct {
	start uint64
	want  int
}

var errQueueClosed = errors.New("queue closed")

// DequeueWithAckId pops one item; a consumer that finds the queue empty
// joins the idle set in the same step.
func (q *Queue) DequeueWithAckId() (any, bool, string) {
	return q.dequeueOne(listAckPopOne)
}

// DequeueBatchWithAckIds moves up to min(len(items), len(ackIDs), 1000) items
// from the queue to pending in one atomic step, each under its own ack ID,
// into items[:n] and ackIDs[:n]. remaining is the queue length right after
// the move. n is 0 with a nil error when the queue is empty, or when no
// capacity was given (remaining is then 0 and means nothing).
//
// A call takes about as many items as this object's previous batch calls
// suggest (the full count on its first call): what the last call left in the
// queue, or once the queue was emptied, as many as were taken since it was
// last seen empty. A trickle thus pops one item per call, cheaply, and a
// burst like the last one is taken in one call. So n can be below the count
// while remaining > 0, and the caller should call again. A call can also
// reuse a failed call's ack IDs and get that call's items back: the next
// call with at least that call's slots does. One failed call is kept at a
// time; the items of another that failed with it stay pending until they are
// requeued. A consumer whose pop comes up short joins the idle set in the
// same step.
func (q *Queue) DequeueBatchWithAckIds(items []any, ackIDs []string) (n, remaining int, err error) {
	return q.dequeueBatch(listAckPop, listAckPopOneLen, items, ackIDs)
}

// dequeueOne pops one item with a lean single-item script (see popOneHead),
// and sets this consumer idle when it finds nothing (a paused miss stays
// out of :idle, spec 6.2).
func (q *Queue) dequeueOne(pop *redis.Script, extraKeys ...string) (item any, ok bool, ackID string) {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot dequeue")
		return nil, false, ""
	}
	consumer := q.consumerID()
	defer func() { q.idle.Store(!ok && !q.Paused()) }()

	r := q.reserve(1, 1)
	ackID = q.ackIDOf(fmt.Sprintf("%020d", r.start), 1)
	g := q.pauseHooks.gen.Load()
	args := append(append([]any{q.vt(), ackID, consumer}, q.cancelArgs()...), q.hbt(), q.ackPrefix)
	keys := q.popKeys(extraKeys...)
	reply, err := pop.Run(q.ctx, q.client, keys, args...).Result()
	for err == nil && spent(reply) { // nothing handed out, so ackID is still free
		reply, err = pop.Run(q.ctx, q.client, keys, args...).Result()
	}
	if err == redis.Nil {
		return nil, false, ""
	}

	if err != nil {
		q.retryPop.CompareAndSwap(nil, r)
		log.Printf("Error dequeuing with ack: %v", err)
		return nil, false, ""
	}

	v, ok := reply.(string)
	if !ok { // 0: paused (pausedOne); the guard found nothing held, so r is free
		q.refused(g)
		return nil, false, ""
	}

	q.beating.Store(q.hbt() > 0)
	return []byte(v), true, ackID
}

// popKeys are the keys of the ack pop scripts (see popHead), with extraKeys
// (a zset's :prio) after the idle set. The rest are read from the end:
// :cancelled, :jobs, :completed, :failed, :attempts (see cancelFn), :paused,
// :started, :heartbeats.
func (q *Queue) popKeys(extraKeys ...string) []string {
	return append(append([]string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey(), q.getIdleKey()}, extraKeys...),
		q.getCancelledKey(), q.getJobsKey(), q.getCompletedKey(), q.getFailedKey(), q.getAttemptsKey(),
		q.getPausedKey(), q.getStartedKey(), q.getHeartbeatKey())
}

// cancelArgs are the pop scripts' arguments for cancelFn: failed retention
// (most, age ms) and the outcome channel prefix.
func (q *Queue) cancelArgs() []any {
	return []any{q.keepFailed.Load(), time.Duration(q.keepFailedAge.Load()).Milliseconds(), q.donePrefix()}
}

// reserve returns the ack IDs for a pop of up to want items, by a call with
// room for limit. The IDs are reserved once per call, so a script go-redis
// re-sends carries them again. After a failed call they are kept (retryPop)
// for the next one: if the failed pop ran anyway, the script hands its items
// back now instead of leaving them pending until the visibility timeout. A
// call with room for fewer than the kept reservation leaves it for a later
// call, and a failure that finds one already kept drops its own: those items
// wait for the timeout.
func (q *Queue) reserve(limit, want int) *reservation {
	r := q.retryPop.Swap(nil)
	if r != nil && r.want > limit {
		q.retryPop.CompareAndSwap(nil, r)
		r = nil
	}
	if r == nil {
		r = &reservation{start: q.ackSeq.Add(1), want: want}
	}
	return r
}

// ackIDOf is the ack ID of item i (from 1) of reservation resv.
func (q *Queue) ackIDOf(resv string, i int) string {
	return fmt.Sprintf("%s:%s:%04d", q.ackPrefix, resv, i)
}

// vt is the visibility timeout in seconds, as the scripts take it (0: none).
func (q *Queue) vt() float64 {
	return max(0, q.getVisibilityTimeout().Seconds())
}

// deadlineFn defines deadline(vt) for scripts: Redis's clock plus vt seconds,
// or 0 when vt is 0. A deadline taken from Redis's clock when the script runs
// counts from when the item is actually popped: one taken by the client when
// it sends the pop starts early by however long the pop waited (a paused or
// busy server, a go-redis re-send) and by any client clock skew, and the
// sweeper could requeue an item its consumer just got. Deadlines are Unix
// seconds, as older versions stored them.
//
// nowms() is Redis's clock in Unix ms, as a string, for start and finish
// times.
//
// TIME is non-deterministic, so Redis 6 configured to replicate scripts
// verbatim (lua-replicate-commands no) rejects writes after it unless the
// script switches to replicating its effects first. Redis 7 always does.
const deadlineFn = `redis.replicate_commands()
local function deadline(vt)
	vt = tonumber(vt)
	if vt <= 0 then return 0 end
	local t = redis.call('TIME')
	return tonumber(t[1]) + tonumber(t[2]) / 1e6 + vt
end
local function nowms()
	local t = redis.call('TIME')
	return string.format('%.0f', tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000))
end
`

// beatFn defines beat() for the pop scripts, after deadlineFn: it sets the
// heartbeat of owner ARGV[#ARGV] (an ack ID prefix) in ZSET KEYS[#KEYS] to
// ARGV[#ARGV-1] seconds past Redis's clock, unless that is 0. A pop that
// hands out items beats in the same step, so a pop that runs late, such as a
// re-send that lands after Close or after another process took the owner for
// dead, beats again, and its items come back one heartbeat timeout later
// instead of after the visibility timeout.
const beatFn = `local function beat()
	local h = deadline(ARGV[#ARGV - 1])
	if h > 0 then redis.call('ZADD', KEYS[#KEYS], h, ARGV[#ARGV]) end
end
`

// dequeueBatch runs pop for up to len(items) items, marking them pending in
// the same step (see popHead), or popOne when it takes one item. It sizes the
// pop by popLen: the length the last call left, or after the queue emptied,
// the items taken since it was last seen empty (an empty call keeps it). A
// trickle then pops one item with the lean script instead of building and
// checking an ack ID for every slot, and a burst like the last one takes one
// call. A guess that is too small costs one more call, and remaining says so.
//
// A consumer whose pop comes up short with the queue empty is idle after it
// (as the script's idle rule; a paused miss stays out). A call whose drop
// budget ran out before it handed out anything (see popGate) runs again, as
// the single pops do, while the queue shrinks, so a backlog of cancelled
// items costs no wait for a wake-up.
func (q *Queue) dequeueBatch(pop, popOne *redis.Script, items []any, ackIDs []string, extraKeys ...string) (n, remaining int, err error) {
	slots := min(len(items), len(ackIDs))
	limit := min(slots, maxPop)
	if limit == 0 {
		return 0, 0, nil
	}

	if q.ctx.Err() != nil {
		return 0, 0, errQueueClosed
	}
	consumer := q.consumerID()
	defer func() {
		if err == nil {
			q.idle.Store(remaining == 0 && n < slots && !q.Paused())
		}
	}()

	r := q.reserve(limit, min(limit, max(1, int(q.popLen.Load()))))
	resv := fmt.Sprintf("%020d", r.start)
	keys := q.popKeys(extraKeys...)
	g := q.pauseHooks.gen.Load()
	tail := append(q.cancelArgs(), q.hbt(), q.ackPrefix)
	script, args := pop, append([]any{r.want, q.vt(), q.ackPrefix, resv, consumer, slots}, tail...)
	if r.want == 1 {
		script, args = popOne, append([]any{q.vt(), q.ackIDOf(resv, 1), consumer, slots}, tail...)
	}
	var reply, popped []any
	for last := -1; ; last = remaining {
		reply, err = script.Run(q.ctx, q.client, keys, args...).Slice()
		if err != nil {
			q.retryPop.CompareAndSwap(nil, r)
			return 0, 0, err
		}

		if len(reply) > 2 { // {0, {}, 1}: paused (pausedBatch); popLen stays for the Resume
			q.refused(g)
			return 0, 0, nil
		}

		popped = reply[1].([]any)
		remaining = int(reply[0].(int64))
		// Nothing handed out with items left: the drop budget ran out (see
		// popGate). Pop again; nothing was handed out, so r is still free.
		// Only while the queue shrinks, so no other empty reply (a hand-back
		// whose first item left pending) can spin.
		if len(popped) > 0 || remaining == 0 || (last >= 0 && remaining >= last) {
			break
		}
	}

	if len(popped) > 0 {
		q.beating.Store(q.hbt() > 0)
	}
	for i, v := range popped {
		items[i] = []byte(v.(string))
		ackIDs[i] = q.ackIDOf(resv, i+1)
	}

	if remaining > 0 {
		q.popRun.Add(int64(len(popped)))
		q.popLen.Store(int64(remaining))
	} else if run := q.popRun.Swap(0) + int64(len(popped)); run > 0 {
		q.popLen.Store(run)
	}

	return len(popped), remaining, nil
}

// requeueLoop and requeueEnd wrap a push of item v back onto the queue. With
// ARGV[1] > 0 they requeue pending items whose visibility deadline is at or
// before Redis's clock (see deadlineFn), else every pending item. An item leaves
// the pending hash in the same script that pushes it, so processes
// requeueing at once can't push it twice.
// ponytail: one script for all expired items blocks Redis for that long;
// batch with ZRANGEBYSCORE LIMIT if pending runs to millions.
const (
	requeueLoop = `redis.replicate_commands()
` + envIDFn + requeuedFn + `local ids
if tonumber(ARGV[1]) > 0 then
	local t = redis.call('TIME')
	ids = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', tonumber(t[1]) + tonumber(t[2]) / 1e6)
else
	ids = redis.call('HKEYS', KEYS[2])
end
local n = 0
for _, id in ipairs(ids) do
	local v = redis.call('HGET', KEYS[2], id)
	redis.call('HDEL', KEYS[2], id)
	redis.call('ZREM', KEYS[3], id)
	if v then
`
	requeueEnd = `
		requeued(v, id)
		n = n + 1
	end
end
return n`
)

// requeuedFn defines requeued(v, id), the bookkeeping of item v going back
// to the queue from processing under ack ID id: it drops id's start time
// from KEYS[4], :started, and counts a job with an envelope id in KEYS[5],
// :attempts (see requeueKeys).
const requeuedFn = `local function requeued(v, id)
	redis.call('HDEL', KEYS[4], id)
	local e = envid(v)
	if e ~= '' then redis.call('HINCRBY', KEYS[5], e, 1) end
end
`

// listRequeue pushes requeued items to the front, ahead of newer work.
var listRequeue = redis.NewScript(requeueLoop + `redis.call('LPUSH', KEYS[1], v)` + requeueEnd)

// The sweep and release scripts take requeueKeys, then the heartbeats ZSET (member: ack ID prefix, score: Unix
// seconds it runs out at), for the sweep its clock, and the presence ZSET
// (see tickBody). repush(v, id) puts item v, pending as id, back at the front
// of the queue, and moveFn's move(id) pushes before it drops the item from
// pending, so a push that fails loses nothing. move returns 1, or 0 when id is no longer pending (its deadline,
// if left behind, still goes).
const (
	listRepush = `local function repush(v, id) redis.call('LPUSH', KEYS[1], v) end
`
	moveFn = `local function move(id)
	local v = redis.call('HGET', KEYS[2], id)
	if v then
		repush(v, id)
		redis.call('HDEL', KEYS[2], id)
		requeued(v, id)
	end
	redis.call('ZREM', KEYS[3], id)
	return v and 1 or 0
end
`
)

// tickBody is the sweep. KEYS end with the heartbeats, their clock, the
// presence ZSET, :paused and :cancelled (see tickKeys). It prunes expired
// cancel marks (spec 6.14), requeues pending items whose visibility deadline
// is past Redis's clock, renews heartbeat ARGV[1] by ARGV[2] seconds (drops
// it when that is 0; only a pop adds one), and requeues the items of owners
// whose heartbeat ran out or who are gone (below). ARGV[3] is the longest
// gap, in seconds, between sweeps that counts as normal. It returns {items
// moved, 1 if ARGV[1]'s heartbeat was missing or had run out, else 0, 1 if
// the queue is paused, else 0}.
//
// The clock is when any process last swept. A longer gap means Redis, or
// every process, stalled: a failover, a restart, a pause. No one could beat
// then, so the heartbeats move on by the gap first, and a stall doesn't make
// every consumer look dead. With no clock yet, no owner is taken for dead.
//
// Presence: a queue object's wake channel is ARGV[4] .. its ack ID prefix
// (see Queue.start), and Redis drops its subscription within
// milliseconds of the process exiting. The sweep that moves the clock on
// counts the subscribers of every owner with a live heartbeat and keeps the
// result in the presence ZSET: 0 once it has seen one, else when it first
// saw none. An owner seen subscribed and then without one for ARGV[5]
// seconds is gone. Its own sweep sets it back to 0, so a process that still
// sweeps is never taken for gone, whatever its connection does. Owners never
// seen subscribed (older versions, which subscribed only distributed queues,
// or a process gone within a second of its first pop) are left to
// their heartbeat, and so is a lost host, whose subscription Redis keeps
// until TCP keepalive drops it.
// ponytail: a dead owner, like a Close (releaseBody), costs a read of every
// pending ID (HKEYS); index pending items by owner if pending runs to millions.
// The presence count covers 1000 owners per sweep; the rest keep heartbeats.
const tickBody = `local hb, clock, pres = KEYS[#KEYS - 4], KEYS[#KEYS - 3], KEYS[#KEYS - 2]
local paused = redis.call('EXISTS', KEYS[#KEYS - 1])
local t = redis.call('TIME')
local now = tonumber(t[1]) + tonumber(t[2]) / 1e6
redis.call('ZREMRANGEBYSCORE', KEYS[#KEYS], '-inf', string.format('%.0f', now * 1000))
local last = tonumber(redis.call('GET', clock))
if last and now - last > tonumber(ARGV[3]) then
	for _, m in ipairs(redis.call('ZRANGE', hb, 0, -1)) do redis.call('ZINCRBY', hb, now - last, m) end
	for _, m in ipairs(redis.call('ZRANGEBYSCORE', pres, '(0', '+inf')) do redis.call('ZINCRBY', pres, now - last, m) end
end
local ticked = not last or now - last >= 0.5 or now < last
if ticked then redis.call('SET', clock, now) end
local own = tonumber(redis.call('ZSCORE', hb, ARGV[1]))
if own then
	if tonumber(ARGV[2]) > 0 then
		redis.call('ZADD', hb, now + tonumber(ARGV[2]), ARGV[1])
		redis.call('ZADD', pres, 'XX', 0, ARGV[1])
	else
		redis.call('ZREM', hb, ARGV[1])
		redis.call('ZREM', pres, ARGV[1])
	end
end
local n = 0
for _, id in ipairs(redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', now)) do n = n + move(id) end
if last then
	local dead, ids = {}, {}
	for _, m in ipairs(redis.call('ZRANGEBYSCORE', hb, '-inf', now, 'LIMIT', 0, 100)) do dead[m] = true end
	if ticked then
		local live, chans = redis.call('ZRANGEBYSCORE', hb, now, '+inf', 'LIMIT', 0, 1000), {}
		for i, m in ipairs(live) do chans[i] = ARGV[4] .. m end
		local subs = #live > 0 and redis.call('PUBSUB', 'NUMSUB', unpack(chans)) or {}
		for i, m in ipairs(live) do
			local s = tonumber(redis.call('ZSCORE', pres, m))
			if subs[2 * i] > 0 then
				if s ~= 0 then redis.call('ZADD', pres, 0, m) end
			elseif s == 0 then redis.call('ZADD', pres, now, m)
			elseif s and now - s >= tonumber(ARGV[5]) then dead[m] = true end
		end
	end
	if next(dead) then
		for _, id in ipairs(redis.call('HKEYS', KEYS[2])) do
			if dead[string.match(id, '^(.-):') or ''] then ids[#ids + 1] = id end
		end
		table.sort(ids)
		for _, id in ipairs(ids) do n = n + move(id) end
		for m in pairs(dead) do
			redis.call('ZREM', hb, m)
			redis.call('ZREM', pres, m)
		end
	end
end
if own and own >= now then return {n, 0, paused} end
return {n, 1, paused}`

// releaseBody requeues every pending item of owner ARGV[1], taking them in
// pop order, and then drops its heartbeat and presence.
const releaseBody = `local p, ids = ARGV[1] .. ':', {}
for _, id in ipairs(redis.call('HKEYS', KEYS[2])) do
	if string.sub(id, 1, #p) == p then ids[#ids + 1] = id end
end
table.sort(ids)
local n = 0
for _, id in ipairs(ids) do n = n + move(id) end
redis.call('ZREM', KEYS[#KEYS - 1], ARGV[1])
redis.call('ZREM', KEYS[#KEYS], ARGV[1])
return n`

// heartbeatScripts builds the sweep and release scripts for a storage's repush.
func heartbeatScripts(repush string) (tick, release *redis.Script) {
	head := "redis.replicate_commands()\n" + envIDFn + requeuedFn + repush + moveFn
	return redis.NewScript(head + tickBody), redis.NewScript(head + releaseBody)
}

var listTick, listRelease = heartbeatScripts(listRepush)

// sweepGap is the longest gap between two sweeps of a key that the sweep
// script takes as normal (see tickBody): above a sweep's one second plus a
// slow call, below minHeartbeat.
const sweepGap = 2 * time.Second

// presenceGrace is how long a queue object may go without a subscriber on
// its wake channel and without a sweep of its own before other processes
// take it for gone (see tickBody). A live owner sweeps every second and
// resubscribes within milliseconds of Redis dropping its connection, so the
// grace only has to outlast one missed sweep (a 2s gap) while it reconnects.
// Sweeps land 1s apart, so a gone owner is reaped on the third one after the
// first that sees it unsubscribed.
const presenceGrace = 2500 * time.Millisecond

// RequeueNackedItems returns un-acknowledged items whose visibility timeout
// has passed (every pending item when it is 0) to the queue.
func (q *Queue) RequeueNackedItems() error {
	_, err := q.requeue(listRequeue)
	return err
}

// sweep runs the sweep script (see tickBody) every second until Close, and
// wakes idle consumers after it moves any item. Once a minute it also trims the
// Job Records (see trimScript). Without it an item whose consumer
// died stayed pending until the next process start. It requeues only items
// past their deadline or held by a dead owner, never every pending item as
// RequeueNackedItems does with no visibility timeout, so it runs whatever
// this queue's timeouts.
// ponytail: one script call per queue per second; tie the tick to the
// timeouts if that load ever shows up.
func (q *Queue) sweep() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for i := 1; ; i++ {
		select {
		case <-q.ctx.Done():
			return
		case <-tick.C:
		}

		if !q.tick() { // client closed: queue is dead
			q.cancel()              // stops the pause loop
			q.Notification.cancel() // else a producer's nudge loop never sees ErrClosed
			return
		}
		q.sweepOutcomes()
		if i%60 == 0 { // ponytail: a minute is fine against hour-long ages
			q.trim()
			if s := q.store(); s != nil {
				s.trimIndex(q.ctx)
			}
		}
	}
}

// tick runs the sweep script once, and returns false once the client is
// closed.
func (q *Queue) tick() bool {
	keys := q.tickKeys()
	g := q.pauseHooks.gen.Load()
	r, err := q.tickScript.Run(q.ctx, q.client, keys, q.ackPrefix, q.hbt(), sweepGap.Seconds(), q.wakePrefix, presenceGrace.Seconds()).Int64Slice()
	if errors.Is(err, redis.ErrClosed) {
		return false // the client was closed without closing this queue
	}
	if err != nil {
		if q.ctx.Err() == nil {
			log.Printf("Error requeueing pending items: %v", err)
		}
		return true
	}

	if r[1] == 1 && q.hbt() > 0 && q.beating.CompareAndSwap(true, false) {
		log.Printf("queue %s: taken for dead (heartbeat ran out, or pub/sub connection and sweep gone); its unacknowledged items may have been requeued to other consumers", q.queueKey)
	}

	q.observe(g, r[2] == 1)
	q.requeued(r[0])
	return true
}

// tickKeys are the sweep script's keys (see tickBody).
func (q *Queue) tickKeys() []string {
	return append(q.requeueKeys(), q.getHeartbeatKey(), q.getHeartbeatKey()+":clock", q.getPresenceKey(), q.getPausedKey(), q.getCancelledKey())
}

// requeued wakes idle consumers after n items went back to the queue.
func (q *Queue) requeued(n int64) {
	if n > 0 {
		q.wake(n)
	}
}

// requeueKeys are the keys the requeue, sweep and release scripts start
// with: queue, pending hash, deadlines, start times, attempts, then
// storageKeys (a zset's :seq and :prio).
func (q *Queue) requeueKeys() []string {
	return append([]string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey(), q.getStartedKey(), q.getAttemptsKey()}, q.storageKeys...)
}

func (q *Queue) requeue(script *redis.Script) (int64, error) {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot requeue idle items")
		return 0, fmt.Errorf("queue closed")
	}

	n, err := script.Run(q.ctx, q.client, q.requeueKeys(), q.vt()).Int64()
	if err != nil {
		log.Printf("Error requeueing pending items: %v", err)
		return 0, fmt.Errorf("error requeueing pending items: %w", err)
	}

	return n, nil
}

// GetNackedItemsCount returns the number of items in the nacked list
func (q *Queue) GetNackedItemsCount() int {
	count, err := q.client.HLen(q.ctx, q.getNackedItemKey()).Result()
	if err != nil {
		return 0
	}
	return int(count)
}

// getNackedItemKey returns the Redis key for the nacked items list
func (q *Queue) getNackedItemKey() string {
	return q.queueKey + ":nacked"
}

// getIdleKey returns the Redis key for the idle consumers of a distributed queue
func (q *Queue) getIdleKey() string {
	return q.queueKey + ":idle"
}

// getPriorityKey returns the Redis key for the priorities of pending items
func (q *Queue) getPriorityKey() string {
	return q.queueKey + ":prio"
}

// getTimeoutKey returns the Redis key for the visibility timeout ZSET
func (q *Queue) getTimeoutKey() string {
	return q.queueKey + ":timeouts"
}

// getHeartbeatKey returns the Redis key for the heartbeats of the queues
// holding pending items
func (q *Queue) getHeartbeatKey() string {
	return q.queueKey + ":heartbeats"
}

// getStartedKey returns the Redis key for when each processing job started
func (q *Queue) getStartedKey() string {
	return q.queueKey + ":started"
}

// getAttemptsKey returns the Redis key for how often each job (by envelope
// id) went back to the queue without an outcome
func (q *Queue) getAttemptsKey() string {
	return q.queueKey + ":attempts"
}

// getJobsKey returns the Redis key for the Job Records
func (q *Queue) getJobsKey() string {
	return q.queueKey + ":jobs"
}

// getCompletedKey returns the Redis key for the completed jobs by finish time
func (q *Queue) getCompletedKey() string {
	return q.queueKey + ":completed"
}

// getFailedKey returns the Redis key for the failed jobs by finish time
func (q *Queue) getFailedKey() string {
	return q.queueKey + ":failed"
}

// getCancelledKey returns the Redis key for the Job IDs with a cancel request
func (q *Queue) getCancelledKey() string {
	return q.queueKey + ":cancelled"
}

// getStatsKey returns the Redis key for the submitted, completed and failed
// counters
func (q *Queue) getStatsKey() string {
	return q.queueKey + ":stats"
}

// getPresenceKey returns the Redis key for whether the owners of pending
// items are still subscribed to their wake channel (see tickBody)
func (q *Queue) getPresenceKey() string {
	return q.queueKey + ":presence"
}

// Close stops the queue and requeues the items it dequeued and has not
// acknowledged, at once, as a broker requeues a closed channel's deliveries:
// an Acknowledge after Close fails anyway. Stop or drain the workers first,
// or jobs still running here run again elsewhere.
//
// It leaves the idle set first, so the wake for the requeued items goes to
// another consumer, and drops outcome watchers without calling them (a
// delivery already under way may still call its fn after Close returns).
func (q *Queue) Close() error {
	var err error
	q.closeOnce.Do(func() {
		q.leave()
		q.cancel() // stops the sweep
		err = q.release()
		q.outcomes.close()
		q.Notification.Stop()
	})
	return err
}

// release requeues this queue's pending items and drops its heartbeat. A pop
// that Redis runs after it beats again (see beatFn), and its items come back
// one heartbeat timeout later.
func (q *Queue) release() error {
	if q.releaseScript == nil || q.ackSeq.Load() == 0 {
		return nil // nothing was ever popped
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n, err := q.releaseScript.Run(ctx, q.client, append(q.requeueKeys(), q.getHeartbeatKey(), q.getPresenceKey()), q.ackPrefix).Int64()
	if errors.Is(err, redis.ErrClosed) {
		return nil // the client is gone: the heartbeat runs out instead
	}
	if err != nil {
		return fmt.Errorf("error requeueing unacknowledged items: %w", err)
	}

	q.requeued(n)
	return nil
}

func (q *Queue) Remove(item any) bool {
	_, err := q.client.LRem(q.ctx, q.queueKey, 1, item).Result()

	return err == nil
}

// Listen blocks until the process gets SIGINT or SIGTERM.
func (q *Queue) Listen() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	<-sigChan
}
