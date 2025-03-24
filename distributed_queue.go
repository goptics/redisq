package redisq

type RedisDistributedQueue struct {
	*RedisQueue
	*Notification
}

func NewRedisDistributedQueue(queueKey string, url string) *RedisDistributedQueue {
	rq := NewRedisQueue(queueKey, url)
	q := &RedisDistributedQueue{
		RedisQueue:   rq,
		Notification: NewNotification(rq.client, queueKey),
	}

	return q
}

func (q *RedisDistributedQueue) Enqueue(item any) bool {
	if message, err := q.toBytes(item); err == nil {
		defer q.Send("enqueued", message)
	}

	return q.RedisQueue.Enqueue(item)
}

func (q *RedisDistributedQueue) Dequeue() (any, bool) {
	item, ok := q.RedisQueue.Dequeue()

	if ok {
		defer q.Send("dequeued", item.([]byte))
	}

	return item, ok
}

func (q *RedisDistributedQueue) Close() error {
	q.Notification.Stop()
	return q.RedisQueue.Close()
}
