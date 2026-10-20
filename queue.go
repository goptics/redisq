package redisq

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lucsky/cuid"
	"github.com/redis/go-redis/v9"
)

type Queue struct {
	client            *redis.Client
	queueKey          string
	mx                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	expiration        time.Duration
	visibilityTimeout time.Duration
	onRequeue         func() // called after the sweeper requeues items; guarded by mx
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
		client:            c,
		queueKey:          queueKey,
		ctx:               ctx,
		cancel:            cancel,
		visibilityTimeout: 5 * time.Minute,
	}
	return q
}

// SetExpiration sets the expiration time for the Queue
func (q *Queue) SetExpiration(expiration time.Duration) {
	q.mx.Lock()
	defer q.mx.Unlock()
	q.expiration = expiration
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
	q.mx.Lock()
	defer q.mx.Unlock()
	q.visibilityTimeout = d
}

// Dequeue removes and returns an item from the queue without acknowledgment
// For reliable processing with acknowledgment, use DequeueWithAck instead
func (q *Queue) Dequeue() (any, bool) {
	q.mx.Lock()
	defer q.mx.Unlock()

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
	q.mx.Lock()
	defer q.mx.Unlock()

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

		pipe := q.client.Pipeline()
		pipe.RPush(q.ctx, q.queueKey, data)

		if q.expiration > 0 {
			pipe.Expire(q.ctx, q.queueKey, q.expiration)
		}

		if _, err := pipe.Exec(q.ctx); err != nil {
			log.Printf("Error enqueueing item: %v", err)
			return false
		}

		return true
	}
}

func (q *Queue) Len() int {
	q.mx.Lock()
	defer q.mx.Unlock()

	length, err := q.client.LLen(q.ctx, q.queueKey).Result()
	if err != nil {
		return 0
	}
	return int(length)
}

func (q *Queue) Purge() {
	q.mx.Lock()
	defer q.mx.Unlock()

	q.client.Del(q.ctx, q.queueKey)
}

func (q *Queue) Values() []any {
	q.mx.Lock()
	defer q.mx.Unlock()

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
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		return fmt.Errorf("queue closed, cannot prepare for acknowledgment")
	default:
		// Store in pending list with the ack ID
		pendingKey := q.getNackedItemKey()
		pipe := q.client.Pipeline()

		pipe.HSet(q.ctx, pendingKey, ackID, item)

		// If visibility timeout is set, track it in ZSET
		if q.visibilityTimeout > 0 {
			timeoutKey := q.getTimeoutKey()
			score := unixSeconds(time.Now().Add(q.visibilityTimeout))
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
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot acknowledge")
		return false
	default:
		pipe := q.client.Pipeline()
		pipe.HDel(q.ctx, q.getNackedItemKey(), ackID)
		pipe.ZRem(q.ctx, q.getTimeoutKey(), ackID)
		pipe.HDel(q.ctx, q.getPriorityKey(), ackID) // priority queues only; a no-op otherwise

		_, err := pipe.Exec(q.ctx)

		if err != nil {
			log.Printf("Error acknowledging item: %v", err)
			return false
		}
		return true
	}
}

// ackPending is the tail of the ack pop scripts: it stores the popped item v
// in the pending hash under ARGV[1], with the same bookkeeping as
// PrepareForFutureAck. ARGV[2] is the visibility deadline (see
// unixSeconds; 0: none).
const ackPending = `
redis.call('HSET', KEYS[2], ARGV[1], v)
if tonumber(ARGV[2]) > 0 then redis.call('ZADD', KEYS[3], ARGV[2], ARGV[1]) end
return v`

// Pop and pending run as one script, so an item is always in the queue or in
// the pending hash: a client that dies between two round trips can't lose it.
var (
	listAckPop = redis.NewScript(`local v = redis.call('LPOP', KEYS[1])
if not v then return false end` + ackPending)
	// zsetAckPop also keeps the popped score in KEYS[4] under the ack ID, so
	// a requeue restores the item's priority.
	zsetAckPop = redis.NewScript(`local r = redis.call('ZPOPMIN', KEYS[1])
local v = r[1]
if not v then return false end
redis.call('HSET', KEYS[4], ARGV[1], r[2])
if #v >= 17 and string.byte(v, 1) == 255 then v = string.sub(v, 18) end` + ackPending)
)

func (q *Queue) DequeueWithAckId() (any, bool, string) {
	return q.dequeueWithAck(listAckPop)
}

// dequeueWithAck pops the next item with pop and marks it pending under a new
// ack ID in the same atomic step.
func (q *Queue) dequeueWithAck(pop *redis.Script, extraKeys ...string) (any, bool, string) {
	q.mx.Lock()
	defer q.mx.Unlock()

	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot dequeue")
		return nil, false, ""
	}

	var deadline float64
	if q.visibilityTimeout > 0 {
		deadline = unixSeconds(time.Now().Add(q.visibilityTimeout))
	}

	ackID := cuid.New()
	keys := append([]string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey()}, extraKeys...)
	v, err := pop.Run(q.ctx, q.client, keys, ackID, deadline).Text()
	if err == redis.Nil {
		return nil, false, ""
	}

	if err != nil {
		log.Printf("Error dequeuing with ack: %v", err)
		return nil, false, ""
	}

	return []byte(v), true, ackID
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

		q.mx.Lock()
		vt := q.visibilityTimeout
		q.mx.Unlock()

		if vt <= 0 {
			continue
		}

		n, err := requeue()
		if errors.Is(err, redis.ErrClosed) {
			return // the client was closed without closing this queue
		}

		if n > 0 {
			q.mx.Lock()
			onRequeue := q.onRequeue
			q.mx.Unlock()

			if onRequeue != nil {
				onRequeue()
			}
		}
	}
}

// setOnRequeue sets the callback the sweeper runs after requeueing items.
func (q *Queue) setOnRequeue(fn func()) {
	q.mx.Lock()
	defer q.mx.Unlock()
	q.onRequeue = fn
}

func (q *Queue) requeue(script *redis.Script, extraKeys ...string) (int64, error) {
	q.mx.Lock()
	defer q.mx.Unlock()

	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot requeue idle items")
		return 0, fmt.Errorf("queue closed")
	}

	var now float64
	if q.visibilityTimeout > 0 {
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
	q.mx.Lock()
	defer q.mx.Unlock()

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
	q.mx.Lock()
	defer q.mx.Unlock()

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
