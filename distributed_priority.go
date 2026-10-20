package redisq

import "github.com/redis/go-redis/v9"

// zsetPushWake is zsetPush plus waking idle consumers, up to one per waiting
// item, in a single round trip. ARGV[3] is the priority and ARGV[4] the wake
// channel prefix (see Queue.enqueueWith for ARGV[1] and ARGV[2]).
var zsetPushWake = redis.NewScript(wakeFn + `local s = redis.call('INCR', KEYS[2])
redis.call('ZADD', KEYS[1], ARGV[3], '\255' .. string.format('%016x', s) .. ARGV[1])
if tonumber(ARGV[2]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return wake(KEYS[3], ARGV[4], redis.call('ZCARD', KEYS[1]))`)

type DistributedPriorityQueue struct {
	*PriorityQueue
	*Notification
}

func newDistributedPriorityQueue(client *redis.Client, queueKey string) *DistributedPriorityQueue {
	q := &DistributedPriorityQueue{
		PriorityQueue: newPriorityQueue(client, queueKey),
		Notification:  newNotification(client, queueKey, "ZCARD"),
	}
	q.setOnRequeue(q.wake)
	defer q.Start()

	return q
}

func (q *DistributedPriorityQueue) Enqueue(item any, priority int) bool {
	keys := []string{q.Notification.queueKey, q.Notification.queueKey + ":seq", q.idleKey}
	return q.enqueueWith(zsetPushWake, keys, item, priority, q.wakePrefix)
}

// Len returns the number of items in the queue; a consumer that finds it
// empty joins the idle set.
func (q *DistributedPriorityQueue) Len() int {
	return q.Notification.len()
}

func (q *DistributedPriorityQueue) Dequeue() (any, bool) {
	item, ok := q.PriorityQueue.Dequeue()

	if !ok {
		q.markIdle()
	}

	return item, ok
}

// DequeueWithAckId pops one item; a consumer that finds the queue empty joins
// the idle set in the same step.
func (q *DistributedPriorityQueue) DequeueWithAckId() (any, bool, string) {
	return q.dequeueOne(zsetAckPopOne, q.consumerID(), q.getPriorityKey())
}

// DequeueBatchWithAckIds pops up to len(items) items (see
// PriorityQueue.DequeueBatchWithAckIds); a consumer whose pop comes up short
// joins the idle set in the same step.
func (q *DistributedPriorityQueue) DequeueBatchWithAckIds(items []any, ackIDs []string) (n, remaining int, err error) {
	return q.dequeueBatch(zsetAckPop, items, ackIDs, q.consumerID(), q.getPriorityKey())
}

func (q *DistributedPriorityQueue) Close() error {
	q.Notification.Stop()
	return q.PriorityQueue.Close()
}
