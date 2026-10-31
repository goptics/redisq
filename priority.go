package redisq

import (
	"log"

	"github.com/redis/go-redis/v9"
)

type PriorityQueue struct {
	*Queue
}

// Members are "\xff" + 16 hex digits of a per-queue sequence + the item. The
// sequence keeps identical items as separate members (a ZSET would merge
// them) and breaks score ties in insertion order, not byte order. \xff never
// starts UTF-8 text and sorts after it, so members stored without the prefix
// by older versions are still read as is, and drain first.
const seqPrefixLen = 17

// zsetPushBody adds item ARGV[1] at score ARGV[3] under the next sequence
// number, and sets the queue to expire in ARGV[2] ms when that is above 0.
const zsetPushBody = `local s = redis.call('INCR', KEYS[2])
redis.call('ZADD', KEYS[1], ARGV[3], '\255' .. string.format('%016x', s) .. ARGV[1])
if tonumber(ARGV[2]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return s`

var (
	zsetPush     = redis.NewScript(zsetPushBody)
	zsetPushOnce = redis.NewScript(once(zsetPushBody))
)

// zsetRemove removes the first member holding item ARGV[1].
var zsetRemove = redis.NewScript(`for _, m in ipairs(redis.call('ZRANGE', KEYS[1], 0, -1)) do
	if m == ARGV[1] or (#m >= 17 and string.byte(m, 1) == 255 and string.sub(m, 18) == ARGV[1]) then
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
	return []string{pq.queueKey, pq.queueKey + ":seq"}
}

func newPriorityQueue(client *redis.Client, queueKey string) *PriorityQueue {
	q := &PriorityQueue{
		Queue: newQueueBase(client, queueKey),
	}
	q.isPriority = true
	q.tickScript, q.releaseScript = zsetTick, zsetRelease
	q.storageKeys = []string{q.queueKey + ":seq", q.getPriorityKey()}
	q.start()
	return q
}

// Enqueue adds an item to the queue with a specified priority
// Lower priority values (closer to 1) will be dequeued first
func (pq *PriorityQueue) Enqueue(item any, priority int) bool {
	return pq.enqueueWith(zsetPush, pq.pushKeys(), item, priority)
}

// EnqueueOnce is Enqueue for a job with an id (see Queue.EnqueueOnce).
func (pq *PriorityQueue) EnqueueOnce(item any, priority int, id string) bool {
	if id == "" || pq.dedupWindow.Load() <= 0 {
		return pq.Enqueue(item, priority)
	}
	return pq.enqueueOnceWith(zsetPushOnce, pq.pushKeys(), item, id, priority)
}

// Dequeue removes and returns the highest priority item from the queue (lowest score)
func (pq *PriorityQueue) Dequeue() (any, bool) {
	select {
	case <-pq.ctx.Done():
		log.Println("queue closed, cannot dequeue")
		return nil, false
	default:
		// Get and remove the highest priority item (lowest score) in a single operation
		results, err := pq.client.ZPopMin(pq.ctx, pq.queueKey).Result()
		if err != nil || len(results) == 0 {
			return nil, false
		}

		return itemOf(results[0].Member.(string)), true
	}
}

// Len returns the number of items in the priority queue
func (pq *PriorityQueue) Len() int {
	length, err := pq.client.ZCard(pq.ctx, pq.queueKey).Result()
	if err != nil {
		return 0
	}
	return int(length)
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

func (pq *PriorityQueue) DequeueWithAckId() (any, bool, string) {
	return pq.dequeueOne(zsetAckPopOne, "", pq.getPriorityKey())
}

// DequeueBatchWithAckIds moves up to len(items) items, lowest score first and
// FIFO within a score, to pending in one atomic step (see
// Queue.DequeueBatchWithAckIds).
func (pq *PriorityQueue) DequeueBatchWithAckIds(items []any, ackIDs []string) (n, remaining int, err error) {
	return pq.dequeueBatch(zsetAckPop, zsetAckPopOneLen, items, ackIDs, "", pq.getPriorityKey())
}

// zsetRequeue re-adds requeued items at the priority they were dequeued at
// (KEYS[5]; 1 for items made pending without one) under a new sequence number.
var zsetRequeue = redis.NewScript(requeueLoop + `local s = redis.call('INCR', KEYS[4])
		local p = redis.call('HGET', KEYS[5], id) or 1
		redis.call('HDEL', KEYS[5], id)
		redis.call('ZADD', KEYS[1], p, '\255' .. string.format('%016x', s) .. v)` + requeueEnd)

// zsetRepush is zsetRequeue's push for the sweep and release scripts (see
// moveFn): KEYS[4] is :seq and KEYS[5] :prio.
const zsetRepush = `local function repush(v, id)
	local s = redis.call('INCR', KEYS[4])
	redis.call('ZADD', KEYS[1], redis.call('HGET', KEYS[5], id) or 1, '\255' .. string.format('%016x', s) .. v)
	redis.call('HDEL', KEYS[5], id)
end
`

var zsetTick, zsetRelease = heartbeatScripts(zsetRepush)

// RequeueNackedItems returns un-acknowledged items whose visibility timeout
// has passed (every pending item when it is 0) to the priority queue.
func (pq *PriorityQueue) RequeueNackedItems() error {
	_, err := pq.requeue(zsetRequeue, pq.queueKey+":seq", pq.getPriorityKey())
	return err
}
