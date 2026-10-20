package redisq

import "github.com/redis/go-redis/v9"

// listPushWake pushes an item and wakes idle consumers in a single round
// trip, up to one per waiting item, so a backlog wakes them all (see
// Queue.enqueueWith for ARGV[1] and ARGV[2]; ARGV[3] is the wake channel
// prefix).
var listPushWake = redis.NewScript(wakeFn + `local n = redis.call('RPUSH', KEYS[1], ARGV[1])
if tonumber(ARGV[2]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return wake(KEYS[2], ARGV[3], n)`)

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

func (q *DistributedQueue) DequeueWithAckId() (any, bool, string) {
	item, ok, ackID := q.Queue.DequeueWithAckId()

	if !ok {
		q.markIdle()
	}

	return item, ok, ackID
}

func (q *DistributedQueue) Close() error {
	q.Notification.Stop()
	return q.Queue.Close()
}
