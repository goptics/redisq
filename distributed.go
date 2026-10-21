package redisq

import "github.com/redis/go-redis/v9"

// listPushWake pushes an item and wakes idle consumers in a single round
// trip, up to one per waiting item, so a backlog wakes them all (see
// Queue.enqueueWith for ARGV[1] and ARGV[2]; ARGV[3] is the wake channel
// prefix).
const listPushWakeBody = `local n = redis.call('RPUSH', KEYS[1], ARGV[1])
if tonumber(ARGV[2]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return wake(KEYS[2], ARGV[3], n)`

var (
	listPushWake     = redis.NewScript(wakeFn + listPushWakeBody)
	listPushWakeOnce = redis.NewScript(wakeFn + onceFn + listPushWakeBody) // see onceFn
)

type DistributedQueue struct {
	*Queue
	*Notification
}

func newDistributedQueue(client *redis.Client, queueKey string) *DistributedQueue {
	q := &DistributedQueue{
		Queue:        newQueue(client, queueKey),
		Notification: newNotification(client, queueKey, "LLEN"),
	}
	q.setOnRequeue(q.wake)
	defer q.Start()

	return q
}

func (q *DistributedQueue) Enqueue(item any) bool {
	keys := []string{q.Notification.queueKey, q.idleKey}
	return q.enqueueWith(listPushWake, keys, item, q.wakePrefix)
}

// EnqueueOnce is Enqueue for a job with an id (see Queue.EnqueueOnce). A
// repeat wakes no one.
func (q *DistributedQueue) EnqueueOnce(item any, id string) bool {
	if id == "" || q.dedupWindow.Load() <= 0 {
		return q.Enqueue(item)
	}
	keys := []string{q.Notification.queueKey, q.idleKey}
	return q.enqueueOnceWith(listPushWakeOnce, keys, item, id, q.wakePrefix)
}

// Len returns the number of items in the queue; a consumer that finds it
// empty joins the idle set.
func (q *DistributedQueue) Len() int {
	return q.Notification.len()
}

func (q *DistributedQueue) Dequeue() (any, bool) {
	item, ok := q.Queue.Dequeue()

	if !ok {
		q.markIdle()
	}

	return item, ok
}

// DequeueWithAckId pops one item; a consumer that finds the queue empty joins
// the idle set in the same step.
func (q *DistributedQueue) DequeueWithAckId() (any, bool, string) {
	return q.Queue.dequeueOne(listAckPopOne, q.consumerID())
}

// DequeueBatchWithAckIds pops up to len(items) items (see
// Queue.DequeueBatchWithAckIds); a consumer whose pop comes up short joins
// the idle set in the same step.
func (q *DistributedQueue) DequeueBatchWithAckIds(items []any, ackIDs []string) (n, remaining int, err error) {
	return q.Queue.dequeueBatch(listAckPop, listAckPopOneLen, items, ackIDs, q.consumerID())
}

func (q *DistributedQueue) Close() error {
	q.Notification.Stop()
	return q.Queue.Close()
}
