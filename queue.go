package redisq

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lucsky/cuid"
	"github.com/redis/go-redis/v9"
)

type Queue struct {
	client   *redis.Client
	queueKey string
	ctx      context.Context
	cancel   context.CancelFunc
	// Config is atomic so no lock is held across a network call: Redis applies
	// each command, pipeline and script on its own.
	expiration        atomic.Int64                // time.Duration
	visibilityTimeout atomic.Int64                // time.Duration
	onRequeue         atomic.Pointer[func(int64)] // called after the sweeper requeues n items
	ackPrefix         string                      // ack IDs are <ackPrefix>:<reservation>:<index>, unique per Queue object
	ackSeq            atomic.Uint64               // last reservation number handed out
	retryPop          atomic.Pointer[reservation] // ack IDs of a pop that failed with an error, reused by the next pop
	popLen            atomic.Int64                // items the next batch pop expects (see dequeueBatch); maxPop until the first
	popRun            atomic.Int64                // items batch pops took since the queue was last seen empty
	isPriority        bool                        // a zset queue, with :prio entries to clear on ack; set before any goroutine starts
	dedupWindow       atomic.Int64                // time.Duration EnqueueOnce remembers an id for
}

func newQueue(c *redis.Client, queueKey string) *Queue {
	q := newQueueBase(c, queueKey)

	// requeue nacked items
	q.RequeueNackedItems()
	go q.sweep(func() (int64, error) { return q.requeue(listRequeue) })

	return q
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
	q.visibilityTimeout.Store(int64(5 * time.Minute))
	q.dedupWindow.Store(int64(5 * time.Minute))
	q.popLen.Store(maxPop)
	return q
}

// newID returns an ID unique across processes. cuid.New's random part comes
// from math/rand seeded with the start second, so processes with the same
// pid and host (containers) started together could share IDs: two consumers
// would then build the same ack IDs, and a pop's re-send guard could hand one
// the items the other holds. crypto/rand rules that out.
func newID() string {
	if id, err := cuid.NewCrypto(crand.Reader); err == nil {
		return id
	}
	return cuid.New()
}

func (q *Queue) getExpiration() time.Duration {
	return time.Duration(q.expiration.Load())
}

func (q *Queue) getVisibilityTimeout() time.Duration {
	return time.Duration(q.visibilityTimeout.Load())
}

// SetExpiration sets the expiration time for the Queue
func (q *Queue) SetExpiration(expiration time.Duration) {
	q.expiration.Store(int64(expiration))
}

// SetAckTimeout sets how long a job can be processing before being requeued.
//
// Deprecated: use SetVisibilityTimeout, which this calls. It used to expire
// the whole pending hash, which deleted every unacknowledged job instead of
// requeueing it. A value <= 0 meant "no expiry" and is ignored: a zero
// visibility timeout would requeue every pending job, even ones in progress.
func (q *Queue) SetAckTimeout(ackTimeout time.Duration) {
	if ackTimeout > 0 {
		q.SetVisibilityTimeout(ackTimeout)
	}
}

// SetVisibilityTimeout sets the visibility timeout for jobs
// This controls how long a job remains invisible in the nacked queue before being candidates for requeuing
func (q *Queue) SetVisibilityTimeout(d time.Duration) {
	q.visibilityTimeout.Store(int64(d))
}

// Dequeue removes and returns an item from the queue without acknowledgment
// For reliable processing with acknowledgment, use DequeueWithAck instead
func (q *Queue) Dequeue() (any, bool) {
	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot dequeue")
		return nil, false
	default:
		result, err := q.client.LPop(q.ctx, q.queueKey).Bytes()
		if err == redis.Nil {
			return nil, false
		}

		if err != nil {
			log.Printf("Error dequeuing: %v", err)
			return nil, false
		}

		return result, true
	}
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

func (q *Queue) Enqueue(item any) bool {
	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot enqueue")
		return false
	default:
		data, err := q.toBytes(item)
		if err != nil {
			log.Printf("Error converting item to bytes: %v", err)
			return false
		}

		if exp := q.getExpiration(); exp > 0 {
			_, err = q.client.Pipelined(q.ctx, func(pipe redis.Pipeliner) error {
				pipe.RPush(q.ctx, q.queueKey, data)
				pipe.Expire(q.ctx, q.queueKey, exp)
				return nil
			})
		} else {
			err = q.client.RPush(q.ctx, q.queueKey, data).Err()
		}

		if err != nil {
			log.Printf("Error enqueueing item: %v", err)
			return false
		}

		return true
	}
}

// SetDedupWindow sets how long EnqueueOnce remembers an id: a push of the
// same id within it stores nothing. Keep it above the client's re-send window,
// or a late re-send can store the item again: with go-redis's default timeouts
// up to 7 sends of a script (3 re-sends as EVALSHA, then up to 4 as EVAL after
// a NOSCRIPT), each after a read timeout, a backoff, a pool wait, a dial and
// a connection's setup, about 4 minutes at worst. 0 or less turns EnqueueOnce into Enqueue. Each id
// costs about 100 bytes of Redis memory for the window.
func (q *Queue) SetDedupWindow(d time.Duration) {
	q.dedupWindow.Store(int64(d))
}

// once wraps a push script body so it stores its item once per id:
// KEYS[#KEYS] is the id's key and ARGV[#ARGV] the dedupe window in ms. A key
// already there means a push of the id (an earlier call, or a copy of this
// one that go-redis re-sent) ran within the window, and nothing is pushed.
// The key is set only after the push: Redis keeps a script's earlier writes
// when a later command fails, so a push that fails (a key of the wrong type)
// leaves the id free for a retry instead of silently taken.
func once(body string) string {
	return `if redis.call('EXISTS', KEYS[#KEYS]) == 1 then return 0 end
local r = (function()
` + body + `
end)()
redis.call('SET', KEYS[#KEYS], '1', 'PX', ARGV[#ARGV])
return r`
}

// listPushOnce is Enqueue's push, once per id (see once), with ARGV[1] the
// item and ARGV[2] the queue expiration in ms (0: none).
var listPushOnce = redis.NewScript(once(`redis.call('RPUSH', KEYS[1], ARGV[1])
if tonumber(ARGV[2]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 1`))

// EnqueueOnce is Enqueue for a job with an id: it stores item unless an item
// with the same id was pushed to this queue within the dedupe window (see
// SetDedupWindow), and returns true in both cases. A retried call, or
// go-redis's re-send of a push whose reply was lost, then stores the item
// once. The id is remembered for the whole window, even if its item is
// handled, removed or purged sooner. An empty id is a plain Enqueue.
func (q *Queue) EnqueueOnce(item any, id string) bool {
	if id == "" || q.dedupWindow.Load() <= 0 {
		return q.Enqueue(item)
	}
	return q.enqueueOnceWith(listPushOnce, []string{q.queueKey}, item, id)
}

// enqueueOnceWith is enqueueWith for a script built with once: it
// adds id's key to keys and the dedupe window to the arguments.
func (q *Queue) enqueueOnceWith(script *redis.Script, keys []string, item any, id string, extra ...any) bool {
	window := max(1, time.Duration(q.dedupWindow.Load()).Milliseconds())
	return q.enqueueWith(script, append(keys, q.queueKey+":dedup:"+id), item, append(extra, window)...)
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

	args := append([]any{data, q.getExpiration().Milliseconds()}, extra...)
	if err := script.Run(q.ctx, q.client, keys, args...).Err(); err != nil {
		log.Printf("Error enqueueing item: %v", err)
		return false
	}

	return true
}

func (q *Queue) Len() int {
	length, err := q.client.LLen(q.ctx, q.queueKey).Result()
	if err != nil {
		return 0
	}
	return int(length)
}

func (q *Queue) Purge() {
	q.client.Del(q.ctx, q.queueKey)
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
// visibility deadline ARGV[3] seconds from now (see deadlineFn).
var prepareAck = redis.NewScript(deadlineFn + `redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
local dl = deadline(ARGV[3])
if dl > 0 then redis.call('ZADD', KEYS[2], dl, ARGV[1]) end
return 1`)

// PrepareForFutureAck adds an item to the pending list for acknowledgment tracking
// Returns an error if the operation fails
func (q *Queue) PrepareForFutureAck(ackID string, item any) error {
	select {
	case <-q.ctx.Done():
		return fmt.Errorf("queue closed, cannot prepare for acknowledgment")
	default:
		keys := []string{q.getNackedItemKey(), q.getTimeoutKey()}
		if err := prepareAck.Run(q.ctx, q.client, keys, ackID, item, q.vt()).Err(); err != nil {
			return fmt.Errorf("error storing item in pending list: %v", err)
		}

		return nil
	}
}

// Acknowledge removes an item from the pending list indicating successful processing
func (q *Queue) Acknowledge(ackID string) bool {
	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot acknowledge")
		return false
	default:
		pipe := q.client.Pipeline()
		pipe.HDel(q.ctx, q.getNackedItemKey(), ackID)
		pipe.ZRem(q.ctx, q.getTimeoutKey(), ackID)
		if q.isPriority {
			pipe.HDel(q.ctx, q.getPriorityKey(), ackID)
		}

		_, err := pipe.Exec(q.ctx)

		if err != nil {
			log.Printf("Error acknowledging item: %v", err)
			return false
		}
		return true
	}
}

// maxPop caps the items one pop call takes. A script unpacks 2*maxPop values
// in one call, which must stay under Lua's 8000-value limit.
const maxPop = 1000

// popHead and popTail wrap the storage-specific part of the ack pop scripts,
// which leaves the popped items, as stored, in `items`.
//
// KEYS: queue, pending hash, deadlines, idle set [, priorities].
// ARGV: want, visibility timeout (seconds; 0: none), ack ID
// prefix, reservation (20-digit start), consumer ID (” if the caller is not
// a distributed consumer), the caller's free slots.
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
const popHead = deadlineFn + `local want, pre, resv = tonumber(ARGV[1]), ARGV[3], ARGV[4]
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
end
return reply(items)`

// popScript builds an ack pop script whose body pops into `items`, for
// storage whose length command is lenCmd.
func popScript(lenCmd, body string) *redis.Script {
	return redis.NewScript(strings.ReplaceAll(popHead+body+popTail, "__LEN__", "'"+lenCmd+"'"))
}

var (
	// listAckPop pops from the head of a list. LRANGE+LTRIM rather than
	// LPOP with a count, which needs Redis 6.2. dequeueBatch sends every
	// one-item call to the lean script, so want is at least 2 here.
	listAckPop = popScript("LLEN", `items = redis.call('LRANGE', KEYS[1], 0, want - 1)
if #items > 0 then redis.call('LTRIM', KEYS[1], #items, -1) end`)
	// zsetAckPop pops the lowest scores, stores the bare items (without the
	// sequence prefix) and keeps each score in KEYS[5] so a requeue restores
	// the item's priority.
	zsetAckPop = popScript("ZCARD", `local r, p = redis.call('ZPOPMIN', KEYS[1], want), {}
for i = 1, #r / 2 do
	local m = r[2*i-1]
	if #m >= 17 and string.byte(m, 1) == 255 then m = string.sub(m, 18) end
	items[i] = m
	p[2*i-1] = ids[i]; p[2*i] = r[2*i]
end
if #items > 0 then redis.call('HSET', KEYS[5], unpack(p)) end`)
)

// popOneHead and popOneTail wrap the lean single-item pop that DequeueWithAckId
// runs, which leaves the popped item, as stored, in v (nil when the queue is
// empty). Same keys, ack IDs and rules as the batch scripts (see popHead),
// with none of the batch bookkeeping: per call it costs what a single pop did
// before batches.
//
// ARGV: visibility timeout (seconds; 0: none), ack ID, consumer ID (” if
// the caller is not a distributed consumer).
const popOneHead = deadlineFn + `local dl = deadline(ARGV[1])
local held = redis.call('HGET', KEYS[2], ARGV[2])
if held then
	if dl > 0 then
		local d = redis.call('ZSCORE', KEYS[3], ARGV[2])
		if not d or tonumber(d) < dl then redis.call('ZADD', KEYS[3], dl, ARGV[2]) end
	end
	return held
end
`

const popOneTail = `
if not v then
	if ARGV[3] ~= '' then redis.call('SADD', KEYS[4], ARGV[3]) end
	return false
end
redis.call('HSET', KEYS[2], ARGV[2], v)
if dl > 0 then redis.call('ZADD', KEYS[3], dl, ARGV[2]) end
return v`

// listPopOne and zsetPopOne pop one item into v (nil when empty). The zset
// one keeps the item's score in KEYS[5] and strips the sequence prefix.
const (
	listPopOne = `local v = redis.call('LPOP', KEYS[1])`
	zsetPopOne = `local r = redis.call('ZPOPMIN', KEYS[1])
local v = r[1]
if v then
	redis.call('HSET', KEYS[5], ARGV[2], r[2])
	if #v >= 17 and string.byte(v, 1) == 255 then v = string.sub(v, 18) end
end`
)

var (
	listAckPopOne = redis.NewScript(popOneHead + listPopOne + popOneTail)
	zsetAckPopOne = redis.NewScript(popOneHead + zsetPopOne + popOneTail)
)

// popOneLenHead and popOneLenTail wrap the lean pop a batch call runs when it
// takes one item: popOneHead's guard and popOneTail's bookkeeping, with the
// batch scripts' reply ({len, items}) and idle rule (see popHead). It skips
// building and checking ack IDs for the slots it would not fill.
//
// ARGV: visibility timeout, ack ID, consumer ID, the caller's free slots.
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
return reply({v})`

func popOneLenScript(lenCmd, body string) *redis.Script {
	return redis.NewScript(strings.ReplaceAll(popOneLenHead+body+popOneLenTail, "__LEN__", "'"+lenCmd+"'"))
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

func (q *Queue) DequeueWithAckId() (any, bool, string) {
	return q.dequeueOne(listAckPopOne, "")
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
// requeued.
func (q *Queue) DequeueBatchWithAckIds(items []any, ackIDs []string) (n, remaining int, err error) {
	return q.dequeueBatch(listAckPop, listAckPopOneLen, items, ackIDs, "")
}

// dequeueOne pops one item with a lean single-item script (see popOneHead).
func (q *Queue) dequeueOne(pop *redis.Script, consumer string, extraKeys ...string) (any, bool, string) {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot dequeue")
		return nil, false, ""
	}

	r := q.reserve(1, 1)
	ackID := q.ackIDOf(fmt.Sprintf("%020d", r.start), 1)
	keys := append([]string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey(), q.getIdleKey()}, extraKeys...)
	v, err := pop.Run(q.ctx, q.client, keys, q.vt(), ackID, consumer).Text()
	if err == redis.Nil {
		return nil, false, ""
	}

	if err != nil {
		q.retryPop.CompareAndSwap(nil, r)
		log.Printf("Error dequeuing with ack: %v", err)
		return nil, false, ""
	}

	return []byte(v), true, ackID
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
`

// dequeueBatch runs pop for up to len(items) items, marking them pending in
// the same step (see popHead), or popOne when it takes one item. It sizes the
// pop by popLen: the length the last call left, or after the queue emptied,
// the items taken since it was last seen empty (an empty call keeps it). A
// trickle then pops one item with the lean script instead of building and
// checking an ack ID for every slot, and a burst like the last one takes one
// call. A guess that is too small costs one more call, and remaining says so.
func (q *Queue) dequeueBatch(pop, popOne *redis.Script, items []any, ackIDs []string, consumer string, extraKeys ...string) (int, int, error) {
	slots := min(len(items), len(ackIDs))
	limit := min(slots, maxPop)
	if limit == 0 {
		return 0, 0, nil
	}

	if q.ctx.Err() != nil {
		return 0, 0, errQueueClosed
	}

	r := q.reserve(limit, min(limit, max(1, int(q.popLen.Load()))))
	resv := fmt.Sprintf("%020d", r.start)
	keys := append([]string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey(), q.getIdleKey()}, extraKeys...)
	var reply []any
	var err error
	if r.want == 1 {
		reply, err = popOne.Run(q.ctx, q.client, keys, q.vt(), q.ackIDOf(resv, 1), consumer, slots).Slice()
	} else {
		reply, err = pop.Run(q.ctx, q.client, keys, r.want, q.vt(), q.ackPrefix, resv, consumer, slots).Slice()
	}
	if err != nil {
		q.retryPop.CompareAndSwap(nil, r)
		return 0, 0, err
	}

	popped := reply[1].([]any)
	for i, v := range popped {
		items[i] = []byte(v.(string))
		ackIDs[i] = q.ackIDOf(resv, i+1)
	}

	remaining := int(reply[0].(int64))
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
local ids
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
		n = n + 1
	end
end
return n`
)

// listRequeue pushes requeued items to the front, ahead of newer work.
var listRequeue = redis.NewScript(requeueLoop + `redis.call('LPUSH', KEYS[1], v)` + requeueEnd)

// RequeueNackedItems returns un-acknowledged items whose visibility timeout
// has passed (every pending item when it is 0) to the queue.
func (q *Queue) RequeueNackedItems() error {
	_, err := q.requeue(listRequeue)
	return err
}

// sweep runs requeue every second until Close, and calls onRequeue after it
// moves any item. Without it an item whose consumer died stayed pending until
// the next process start. It skips a zero visibility timeout, where requeue
// takes every pending item, including ones still being processed.
// ponytail: one script call per queue per second; tie the tick to the
// visibility timeout if that load ever shows up.
func (q *Queue) sweep(requeue func() (int64, error)) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-q.ctx.Done():
			return
		case <-tick.C:
		}

		if q.getVisibilityTimeout() <= 0 {
			continue
		}

		n, err := requeue()
		if errors.Is(err, redis.ErrClosed) {
			return // the client was closed without closing this queue
		}

		if n > 0 {
			if onRequeue := q.onRequeue.Load(); onRequeue != nil {
				(*onRequeue)(n)
			}
		}
	}
}

// setOnRequeue sets the callback the sweeper runs after requeueing items.
func (q *Queue) setOnRequeue(fn func(n int64)) {
	q.onRequeue.Store(&fn)
}

func (q *Queue) requeue(script *redis.Script, extraKeys ...string) (int64, error) {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot requeue idle items")
		return 0, fmt.Errorf("queue closed")
	}

	keys := append([]string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey()}, extraKeys...)
	n, err := script.Run(q.ctx, q.client, keys, q.vt()).Int64()
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

func (q *Queue) Close() error {
	q.cancel() // Cancel context to stop notification listener
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
