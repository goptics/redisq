package redisq

import (
	"context"
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
	ackPrefix         string                      // ack IDs are <ackPrefix>:<n>, unique per Queue object
	ackSeq            atomic.Uint64               // last reservation number handed out
	retryPop          atomic.Pointer[reservation] // ack IDs of a pop that failed with an error, reused by the next pop
	isPriority        bool                        // a zset queue, with :prio entries to clear on ack; set before any goroutine starts
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
		ackPrefix: cuid.New(),
	}
	q.visibilityTimeout.Store(int64(5 * time.Minute))
	return q
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

// PrepareForFutureAck adds an item to the pending list for acknowledgment tracking
// Returns an error if the operation fails
func (q *Queue) PrepareForFutureAck(ackID string, item any) error {
	select {
	case <-q.ctx.Done():
		return fmt.Errorf("queue closed, cannot prepare for acknowledgment")
	default:
		// Store in pending list with the ack ID. MULTI/EXEC, so a lost reply
		// can't leave a pending entry without its deadline.
		pendingKey := q.getNackedItemKey()
		pipe := q.client.TxPipeline()

		pipe.HSet(q.ctx, pendingKey, ackID, item)

		// If visibility timeout is set, track it in ZSET
		if vt := q.getVisibilityTimeout(); vt > 0 {
			timeoutKey := q.getTimeoutKey()
			score := unixSeconds(time.Now().Add(vt))
			pipe.ZAdd(q.ctx, timeoutKey, redis.Z{
				Score:  score,
				Member: ackID,
			})
		}

		_, err := pipe.Exec(q.ctx)
		if err != nil {
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
// ARGV: want, deadline (Unix seconds; 0: no visibility timeout), ack ID
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
// leading run of those items with their deadlines raised to this call's. A
// batch shares one deadline, so a requeue moves all of it or none.
//
// A pop that leaves the queue empty while the caller had free slots left
// makes a distributed consumer join the idle set, in the same step, so an
// enqueue right after it wakes it.
const popHead = `local want, pre, resv = tonumber(ARGV[1]), ARGV[3], ARGV[4]
local ids = {}
for i = 1, want do ids[i] = pre .. ':' .. resv .. ':' .. string.format('%04d', i) end
local held = redis.call('HMGET', KEYS[2], unpack(ids))
for i = 1, want do
	if held[i] then
		local items = {}
		for j = 1, want do
			if not held[j] then break end
			items[j] = held[j]
			if tonumber(ARGV[2]) > 0 then
				local d = redis.call('ZSCORE', KEYS[3], ids[j])
				if not d or tonumber(d) < tonumber(ARGV[2]) then redis.call('ZADD', KEYS[3], ARGV[2], ids[j]) end
			end
		end
		return {redis.call(__LEN__, KEYS[1]), items}
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
		z[2*i-1] = ARGV[2]; z[2*i] = ids[i]
	end
	redis.call('HSET', KEYS[2], unpack(h))
	if tonumber(ARGV[2]) > 0 then redis.call('ZADD', KEYS[3], unpack(z)) end
end
local len = redis.call(__LEN__, KEYS[1])
if ARGV[5] ~= '' and len == 0 and n < tonumber(ARGV[6]) then redis.call('SADD', KEYS[4], ARGV[5]) end
return {len, items}`

// popScript builds an ack pop script whose body pops into `items`, for
// storage whose length command is lenCmd.
func popScript(lenCmd, body string) *redis.Script {
	return redis.NewScript(strings.ReplaceAll(popHead+body+popTail, "__LEN__", "'"+lenCmd+"'"))
}

var (
	// listAckPop pops from the head of a list. LRANGE+LTRIM rather than
	// LPOP with a count, which needs Redis 6.2.
	listAckPop = popScript("LLEN", `if want == 1 then
	local v = redis.call('LPOP', KEYS[1])
	if v then items[1] = v end
else
	items = redis.call('LRANGE', KEYS[1], 0, want - 1)
	if #items > 0 then redis.call('LTRIM', KEYS[1], #items, -1) end
end`)
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

// reservation is the ack IDs <prefix>:<start>:0001..<start>:<want> of one pop
// call; start is unique per Queue object.
type reservation struct {
	start uint64
	want  int
}

var errQueueClosed = errors.New("queue closed")

func (q *Queue) DequeueWithAckId() (any, bool, string) {
	return q.dequeueOne(listAckPop, "")
}

// DequeueBatchWithAckIds moves up to min(len(items), len(ackIDs), 1000) items
// from the queue to pending in one atomic step, each under its own ack ID,
// into items[:n] and ackIDs[:n]. remaining is the queue length right after
// the move. n is 0 with a nil error when the queue is empty, or when no
// capacity was given (remaining is then 0 and means nothing). n can be below
// that count while remaining > 0 only right after a failed call: the next call
// of at least that call's size reuses its ack IDs and gets its items back.
func (q *Queue) DequeueBatchWithAckIds(items []any, ackIDs []string) (n, remaining int, err error) {
	return q.dequeueBatch(listAckPop, items, ackIDs, "")
}

// dequeueOne is a batch of one.
func (q *Queue) dequeueOne(pop *redis.Script, consumer string, extraKeys ...string) (any, bool, string) {
	var items [1]any
	var ackIDs [1]string

	n, _, err := q.dequeueBatch(pop, items[:], ackIDs[:], consumer, extraKeys...)
	if err != nil {
		log.Printf("Error dequeuing with ack: %v", err)
		return nil, false, ""
	}

	if n == 0 {
		return nil, false, ""
	}

	return items[0], true, ackIDs[0]
}

// dequeueBatch runs pop for up to len(items) items, marking them pending in
// the same step (see popHead).
func (q *Queue) dequeueBatch(pop *redis.Script, items []any, ackIDs []string, consumer string, extraKeys ...string) (int, int, error) {
	want := min(len(items), len(ackIDs), maxPop)
	if want == 0 {
		return 0, 0, nil
	}

	if q.ctx.Err() != nil {
		return 0, 0, errQueueClosed
	}

	var deadline float64
	if vt := q.getVisibilityTimeout(); vt > 0 {
		deadline = unixSeconds(time.Now().Add(vt))
	}

	// The ack IDs are reserved once per call, so a script go-redis re-sends
	// carries them again. After a failed call they are kept for the next one:
	// if the failed pop ran anyway, popHead hands its items back now instead
	// of leaving them pending until the visibility timeout. A call smaller
	// than the kept reservation leaves it for a later call, and a failure
	// that finds one already kept drops its own: those items wait for the
	// timeout.
	r := q.retryPop.Swap(nil)
	if r != nil && r.want > want {
		q.retryPop.CompareAndSwap(nil, r)
		r = nil
	}
	if r == nil {
		r = &reservation{start: q.ackSeq.Add(1), want: want}
	}

	resv := fmt.Sprintf("%020d", r.start)
	keys := append([]string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey(), q.getIdleKey()}, extraKeys...)
	reply, err := pop.Run(q.ctx, q.client, keys, r.want, deadline, q.ackPrefix, resv, consumer, want).Slice()
	if err != nil {
		q.retryPop.CompareAndSwap(nil, r)
		return 0, 0, err
	}

	popped := reply[1].([]any)
	for i, v := range popped {
		items[i] = []byte(v.(string))
		ackIDs[i] = fmt.Sprintf("%s:%s:%04d", q.ackPrefix, resv, i+1)
	}

	return len(popped), int(reply[0].(int64)), nil
}

// requeueLoop and requeueEnd wrap a push of item v back onto the queue. With
// ARGV[1] > 0 they requeue pending items whose visibility deadline is at or
// before ARGV[1] (see unixSeconds), else every pending item. An item leaves
// the pending hash in the same script that pushes it, so processes
// requeueing at once can't push it twice.
// ponytail: one script for all expired items blocks Redis for that long;
// batch with ZRANGEBYSCORE LIMIT if pending runs to millions.
const (
	requeueLoop = `
local ids
if tonumber(ARGV[1]) > 0 then
	ids = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', ARGV[1])
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

	var now float64
	if q.getVisibilityTimeout() > 0 {
		now = unixSeconds(time.Now())
	}

	keys := append([]string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey()}, extraKeys...)
	n, err := script.Run(q.ctx, q.client, keys, now).Int64()
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

// unixSeconds is t in Unix seconds with millisecond precision, the unit of
// visibility deadlines. Whole seconds would let the sweeper requeue a job up
// to a second early, while it is still being processed. Deadlines stored in
// whole seconds by older versions compare correctly.
func unixSeconds(t time.Time) float64 {
	return float64(t.UnixMilli()) / 1000
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
