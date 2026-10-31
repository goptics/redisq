package redisq

import (
	"time"

	"github.com/redis/go-redis/v9"
)

type queues struct {
	client *redis.Client
}

type Queues interface {
	NewQueue(queueKey string) *Queue
	NewPriorityQueue(queueKey string) *PriorityQueue
	NewDistributedQueue(queueKey string) *DistributedQueue
	NewDistributedPriorityQueue(queueKey string) *DistributedPriorityQueue
	Close() error
}

func New(url string) Queues {
	opts, err := redis.ParseURL(url)
	if err != nil {
		panic(err)
	}

	// Since 9.7, go-redis raised its default timeouts and dials up to 5 times,
	// which stretches the client's re-send window past the 5-minute visibility
	// timeout and dedupe window. Keep the go-redis 9.7 timings unless the URL
	// sets them. The write and pool timeouts follow the read timeout.
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 3 * time.Second
	}
	if opts.MaxRetryBackoff == 0 {
		opts.MaxRetryBackoff = 512 * time.Millisecond
	}
	opts.DialerRetries = 1 // one dial attempt; a URL can't set this

	client := redis.NewClient(opts)

	return &queues{
		client: client,
	}
}

func (qs *queues) NewQueue(queueKey string) *Queue {
	q := newQueue(qs.client, queueKey)
	go q.drain()
	return q
}

func (qs *queues) NewPriorityQueue(queueKey string) *PriorityQueue {
	q := newPriorityQueue(qs.client, queueKey)
	go q.drain()
	return q
}

func (qs *queues) NewDistributedQueue(queueKey string) *DistributedQueue {
	return newDistributedQueue(qs.client, queueKey)
}

func (qs *queues) NewDistributedPriorityQueue(queueKey string) *DistributedPriorityQueue {
	return newDistributedPriorityQueue(qs.client, queueKey)
}

func (qs *queues) Close() error {
	return qs.client.Close()
}
