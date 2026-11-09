package redisq

import (
	"github.com/redis/go-redis/v9"
)

// PriorityQueue is Queue with a priority per item: lowest score first, FIFO
// within a score.
type PriorityQueue struct {
	*Queue
}

// Members are "\xff" + 16 hex digits of a per-queue sequence + the item. The
// sequence keeps identical items as separate members (a ZSET would merge
// them) and breaks score ties in insertion order, not byte order. \xff never
// starts UTF-8 text and sorts after it, so members stored without the prefix
// by older versions are still read as is, and drain first.
const seqPrefixLen = 17

// bareFn defines bare(m): the item stored in member m (see itemOf).
const bareFn = `local function bare(m)
	if #m >= 17 and string.byte(m, 1) == 255 then return string.sub(m, 18) end
	return m
end
`

// zsetPushWakeBody adds item ARGV[1] at score ARGV[3] under the next
// sequence number (KEYS[2], :seq), sets the queue to expire in ARGV[2] ms
// when that is above 0, wakes idle consumers of KEYS[3], up to one per
// waiting item, on channel prefix ARGV[4], and counts the job as submitted
// in KEYS[4], :stats (see listPushWakeBody).
const zsetPushWakeBody = `local s = redis.call('INCR', KEYS[2])
redis.call('ZADD', KEYS[1], ARGV[3], '\255' .. string.format('%016x', s) .. ARGV[1])
if tonumber(ARGV[2]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
redis.call('HINCRBY', KEYS[4], 'submitted', 1)
return wake(KEYS[3], ARGV[4], redis.call('ZCARD', KEYS[1]))`

var (
	zsetPushWake     = redis.NewScript(wakeFn + zsetPushWakeBody)
	zsetPushWakeOnce = redis.NewScript(wakeFn + once(zsetPushWakeBody))
)

// zsetRemove removes the first member holding item ARGV[1].
var zsetRemove = redis.NewScript(bareFn + `for _, m in ipairs(redis.call('ZRANGE', KEYS[1], 0, -1)) do
	if m == ARGV[1] or bare(m) == ARGV[1] then
		return redis.call('ZREM', KEYS[1], m)
	end
end
return 0`)

// itemOf returns the item stored in member m.
func itemOf(m string) []byte {
	if len(m) >= seqPrefixLen && m[0] == 0xff {
		return []byte(m[seqPrefixLen:])
	}
	return []byte(m)
}

func (pq *PriorityQueue) pushKeys() []string {
	return []string{pq.queueKey, pq.queueKey + ":seq", pq.getIdleKey(), pq.getStatsKey()}
}

func newPriorityQueue(client *redis.Client, queueKey string, scope string) *PriorityQueue {
	q := &PriorityQueue{
		Queue: newQueueBase(client, queueKey),
	}
	q.scope = scope
	q.isPriority = true
	q.tickScript, q.releaseScript = zsetTick, zsetRelease
	q.storageKeys = []string{q.queueKey + ":seq", q.getPriorityKey()}
	q.start()
	return q
}

// Enqueue adds an item to the queue with a specified priority and wakes idle
// consumers, as Queue.Enqueue does. Lower priority values (closer to 1) will
// be dequeued first.
func (pq *PriorityQueue) Enqueue(item any, priority int) bool {
	return pq.enqueueWith(zsetPushWake, pq.pushKeys(), item, priority, pq.wakePrefix)
}

// EnqueueOnce is Enqueue for a job with an id (see Queue.EnqueueOnce). A
// repeat wakes no one.
func (pq *PriorityQueue) EnqueueOnce(item any, priority int, id string) bool {
	if id == "" {
		return pq.Enqueue(item, priority)
	}
	return pq.enqueueOnceWith(zsetPushWakeOnce, pq.pushKeys(), item, id, priority, pq.wakePrefix)
}

// Dequeue removes and returns the highest priority item from the queue
// (lowest score). A consumer that finds the queue empty joins the idle set.
func (pq *PriorityQueue) Dequeue() (any, bool) {
	return pq.rawPop(zsetRawPop) // the script strips the sequence prefix
}

func (pq *PriorityQueue) Remove(item any) bool {
	data, err := pq.toBytes(item)
	if err != nil {
		return false
	}

	return zsetRemove.Run(pq.ctx, pq.client, []string{pq.queueKey}, data).Err() == nil
}

// Values returns all items in the priority queue ordered by priority (highest to lowest)
func (pq *PriorityQueue) Values() []any {
	results, err := pq.client.ZRangeWithScores(pq.ctx, pq.queueKey, 0, -1).Result()
	if err != nil {
		return []any{}
	}

	values := make([]any, 0, len(results))
	for _, result := range results {
		values = append(values, itemOf(result.Member.(string)))
	}

	return values
}

// DequeueWithAckId pops the highest priority item; a consumer that finds the
// queue empty joins the idle set in the same step.
func (pq *PriorityQueue) DequeueWithAckId() (any, bool, string) {
	return pq.dequeueOne(zsetAckPopOne, pq.getPriorityKey())
}

// DequeueBatchWithAckIds moves up to len(items) items, lowest score first and
// FIFO within a score, to pending in one atomic step (see
// Queue.DequeueBatchWithAckIds).
func (pq *PriorityQueue) DequeueBatchWithAckIds(items []any, ackIDs []string) (n, remaining int, err error) {
	return pq.dequeueBatch(zsetAckPop, zsetAckPopOneLen, items, ackIDs, pq.getPriorityKey())
}

// zsetRequeue re-adds requeued items at the priority they were dequeued at
// (KEYS[7]; 1 for items made pending without one) under a new sequence
// number from KEYS[6] (see requeueKeys).
var zsetRequeue = redis.NewScript(requeueLoop + `local s = redis.call('INCR', KEYS[6])
		local p = redis.call('HGET', KEYS[7], id) or 1
		redis.call('HDEL', KEYS[7], id)
		redis.call('ZADD', KEYS[1], p, '\255' .. string.format('%016x', s) .. v)` + requeueEnd)

// zsetRepush is zsetRequeue's push for the sweep and release scripts (see
// moveFn): KEYS[6] is :seq and KEYS[7] :prio (see requeueKeys).
const zsetRepush = `local function repush(v, id)
	local s = redis.call('INCR', KEYS[6])
	redis.call('ZADD', KEYS[1], redis.call('HGET', KEYS[7], id) or 1, '\255' .. string.format('%016x', s) .. v)
	redis.call('HDEL', KEYS[7], id)
end
`

var zsetTick, zsetRelease = heartbeatScripts(zsetRepush)

// RequeueNackedItems returns un-acknowledged items whose visibility timeout
// has passed (every pending item when it is 0) to the priority queue.
func (pq *PriorityQueue) RequeueNackedItems() error {
	_, err := pq.requeue(zsetRequeue)
	return err
}
