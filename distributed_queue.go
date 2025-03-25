package redisq

import "github.com/redis/go-redis/v9"

type DistributedQueue struct {
	*Queue
	*Notification
}

func newDistributedQueue(client *redis.Client, queueKey string) *DistributedQueue {
	q := &DistributedQueue{
		Queue:        newQueue(client, queueKey),
		Notification: newNotification(client, queueKey),
	}
	defer q.Start()

	return q
}

func (q *DistributedQueue) Enqueue(item any) bool {
	if message, err := q.toBytes(item); err == nil {
		defer q.Send("enqueued", message)
	}

	return q.Queue.Enqueue(item)
}

func (q *DistributedQueue) Dequeue() (any, bool) {
	item, ok := q.Queue.Dequeue()

	if ok {
		defer q.Send("dequeued", item.([]byte))
	}

	return item, ok
}

func (q *DistributedQueue) Close() error {
	q.Notification.Stop()
	return q.Queue.Close()
}
