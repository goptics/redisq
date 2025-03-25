package redisq

import (
	"log"

	"github.com/redis/go-redis/v9"
)

type PriorityQueue struct {
	*Queue
}

func newPriorityQueue(client *redis.Client, queueKey string) *PriorityQueue {
	return &PriorityQueue{
		Queue: newQueue(client, queueKey),
	}
}

// Enqueue adds an item to the queue with a specified priority
// Lower priority values (closer to 1) will be dequeued first
func (pq *PriorityQueue) Enqueue(item any, priority int) bool {
	pq.mx.Lock()
	defer pq.mx.Unlock()

	select {
	case <-pq.ctx.Done():
		log.Println("queue closed, cannot enqueue")
		return false
	default:
		data, err := pq.toBytes(item)
		if err != nil {
			log.Printf("Error converting item to bytes: %v", err)
			return false
		}

		pipe := pq.client.Pipeline()
		pipe.ZAdd(pq.ctx, pq.queueKey, redis.Z{
			Score:  float64(priority),
			Member: data,
		})

		if pq.expiration > 0 {
			pipe.Expire(pq.ctx, pq.queueKey, pq.expiration)
		}

		if _, err := pipe.Exec(pq.ctx); err != nil {
			log.Printf("Error enqueueing item with priority: %v", err)
			return false
		}

		return true
	}
}

// Dequeue removes and returns the highest priority item from the queue (lowest score)
func (pq *PriorityQueue) Dequeue() (any, bool) {
	pq.mx.Lock()
	defer pq.mx.Unlock()

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

		// Convert string back to []byte to maintain consistency with Queue interface
		return []byte(results[0].Member.(string)), true
	}
}

// Len returns the number of items in the priority queue
func (pq *PriorityQueue) Len() int {
	pq.mx.Lock()
	defer pq.mx.Unlock()

	length, err := pq.client.ZCard(pq.ctx, pq.queueKey).Result()
	if err != nil {
		return 0
	}
	return int(length)
}

// Values returns all items in the priority queue ordered by priority (highest to lowest)
func (pq *PriorityQueue) Values() []any {
	pq.mx.Lock()
	defer pq.mx.Unlock()

	results, err := pq.client.ZRangeWithScores(pq.ctx, pq.queueKey, 0, -1).Result()
	if err != nil {
		return []any{}
	}

	values := make([]any, 0, len(results))
	for _, result := range results {
		// Convert string back to []byte
		values = append(values, []byte(result.Member.(string)))
	}

	return values
}
