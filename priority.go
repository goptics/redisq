package redisq

import (
	"fmt"
	"log"
	"time"

	"github.com/lucsky/cuid"
	"github.com/redis/go-redis/v9"
)

type PriorityQueue struct {
	*Queue
}

func newPriorityQueue(client *redis.Client, queueKey string) *PriorityQueue {
	q := &PriorityQueue{
		Queue: newQueueBase(client, queueKey),
	}

	q.RequeueNackedItems()
	return q
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

func (pq *PriorityQueue) Remove(item any) bool {
	pq.mx.Lock()
	defer pq.mx.Unlock()

	_, err := pq.client.ZRem(pq.ctx, pq.queueKey, item).Result()

	return err == nil
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

func (pq *PriorityQueue) DequeueWithAckId() (any, bool, string) {
	v, ok := pq.Dequeue()

	if !ok {
		return nil, false, ""
	}

	// Prepare for acknowledgment
	ackID := cuid.New()
	err := pq.PrepareForFutureAck(ackID, v)

	if err != nil {
		log.Printf("Error preparing for acknowledgment: %v", err)
		return nil, false, ""
	}

	return v, true, ackID
}

// RequeueNackedItems checks for un-acknowledged items in the nacked list
// and returns them to the priority queue
func (pq *PriorityQueue) RequeueNackedItems() error {
	pq.mx.Lock()
	defer pq.mx.Unlock()

	select {
	case <-pq.ctx.Done():
		log.Println("queue closed, cannot requeue idle items")
		return fmt.Errorf("queue closed")
	default:
		var pendingItems map[string]string
		var err error

		// If visibility timeout is used, only get expired items
		if pq.visibilityTimeout > 0 {
			// Find items with score <= now
			now := float64(time.Now().Unix())
			vals, err := pq.client.ZRangeByScore(pq.ctx, pq.getTimeoutKey(), &redis.ZRangeBy{
				Min: "-inf",
				Max: fmt.Sprintf("%f", now),
			}).Result()

			if err != nil {
				log.Printf("Error getting expired pending items: %v", err)
				return fmt.Errorf("error getting expired pending items: %v", err)
			}

			if len(vals) == 0 {
				return nil
			}

			if len(vals) > 0 {
				items, err := pq.client.HMGet(pq.ctx, pq.getNackedItemKey(), vals...).Result()
				if err != nil {
					log.Printf("Error fetching values for expired items: %v", err)
					return err
				}

				pendingItems = make(map[string]string)
				for i, id := range vals {
					if items[i] != nil {
						switch v := items[i].(type) {
						case string:
							pendingItems[id] = v
						case []byte:
							pendingItems[id] = string(v)
						}
					}
				}
			}

		} else {
			// Old behavior: Get all pending items
			pendingItems, err = pq.client.HGetAll(pq.ctx, pq.getNackedItemKey()).Result()
			if err != nil {
				log.Printf("Error getting pending items: %v", err)
				return fmt.Errorf("error getting pending items: %v", err)
			}
		}

		for ackID, item := range pendingItems {
			// Move from pending back to the priority queue
			// Using ZAdd with score 1 (High Priority) to requeue
			pipe := pq.client.Pipeline()
			pipe.HDel(pq.ctx, pq.getNackedItemKey(), ackID)
			if pq.visibilityTimeout > 0 {
				pipe.ZRem(pq.ctx, pq.getTimeoutKey(), ackID)
			}

			// We convert the string item back to bytes for storage if needed, but ZAdd takes interface{}
			// Redis driver handles string/bytes fine.
			pipe.ZAdd(pq.ctx, pq.queueKey, redis.Z{
				Score:  1, // Default high priority for requeued items
				Member: item,
			})

			if _, err := pipe.Exec(pq.ctx); err != nil {
				log.Printf("Error requeueing idle item: %v", err)
				continue
			}
		}

		return nil
	}
}
