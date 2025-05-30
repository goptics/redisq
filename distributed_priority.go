package redisq

import "github.com/redis/go-redis/v9"

type DistributedPriorityQueue struct {
	*PriorityQueue
	*Notification
}

func newDistributedPriorityQueue(client *redis.Client, queueKey string) *DistributedPriorityQueue {
	q := &DistributedPriorityQueue{
		PriorityQueue: newPriorityQueue(client, queueKey),
		Notification:  newNotification(client, queueKey),
	}
	defer q.Start()

	return q
}

func (q *DistributedPriorityQueue) Enqueue(item any, priority int) bool {
	defer q.Send("enqueued")

	return q.PriorityQueue.Enqueue(item, priority)
}

func (q *DistributedPriorityQueue) Dequeue() (any, bool) {
	item, ok := q.PriorityQueue.Dequeue()

	if ok {
		defer q.Send("dequeued")
	}

	return item, ok
}

func (q *DistributedPriorityQueue) Close() error {
	q.Notification.Stop()
	return q.PriorityQueue.Close()
}
