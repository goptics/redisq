package redisq

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

type Queue struct {
	client     *redis.Client
	queueKey   string
	mx         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	expiration time.Duration
	ackTimeout time.Duration
}

func newQueue(c *redis.Client, queueKey string) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		client:   c,
		queueKey: queueKey,
		ctx:      ctx,
		cancel:   cancel,
	}

	// requeue nacked items
	q.requeueNackedItems()

	return q
}

// SetExpiration sets the expiration time for the Queue
func (q *Queue) SetExpiration(expiration time.Duration) {
	q.mx.Lock()
	defer q.mx.Unlock()
	q.expiration = expiration
}

// SetAckTimeout sets the acknowledgment timeout for jobs
// This controls how long a job can be processing before being requeued
func (q *Queue) SetAckTimeout(ackTimeout time.Duration) {
	q.mx.Lock()
	defer q.mx.Unlock()
	q.ackTimeout = ackTimeout
}

// Dequeue removes and returns an item from the queue without acknowledgment
// For reliable processing with acknowledgment, use DequeueWithAck instead
func (q *Queue) Dequeue() (any, bool) {
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot dequeue")
		return nil, false
	default:
		result, err := q.client.LPop(q.ctx, q.queueKey).Bytes()
		if err == redis.Nil {
			return nil, false
		}

		if err != nil {
			log.Printf("Error dequeuing: %v", err)
			return nil, false
		}

		return result, true
	}
}

func (q *Queue) toBytes(item any) ([]byte, error) {
	var data []byte
	switch v := item.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return nil, fmt.Errorf("unsupported type: %T", v)
	}
	return data, nil
}

func (q *Queue) Enqueue(item any) bool {
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot enqueue")
		return false
	default:
		data, err := q.toBytes(item)
		if err != nil {
			log.Printf("Error converting item to bytes: %v", err)
			return false
		}

		pipe := q.client.Pipeline()
		pipe.RPush(q.ctx, q.queueKey, data)

		if q.expiration > 0 {
			pipe.Expire(q.ctx, q.queueKey, q.expiration)
		}

		if _, err := pipe.Exec(q.ctx); err != nil {
			log.Printf("Error enqueueing item: %v", err)
			return false
		}

		return true
	}
}

func (q *Queue) Len() int {
	q.mx.Lock()
	defer q.mx.Unlock()

	length, err := q.client.LLen(q.ctx, q.queueKey).Result()
	if err != nil {
		return 0
	}
	return int(length)
}

func (q *Queue) Purge() {
	q.mx.Lock()
	defer q.mx.Unlock()

	q.client.Del(q.ctx, q.queueKey)
}

func (q *Queue) Values() []any {
	q.mx.Lock()
	defer q.mx.Unlock()

	results, err := q.client.LRange(q.ctx, q.queueKey, 0, -1).Result()
	if err != nil {
		return []any{}
	}

	values := make([]any, 0, len(results))
	for _, result := range results {
		// Just convert the string to bytes without base64 decoding
		values = append(values, []byte(result))
	}

	return values
}

// PrepareForAck adds an item to the pending list for acknowledgment tracking
// Returns an error if the operation fails
func (q *Queue) PrepareForAck(ackID string, item any) error {
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		return fmt.Errorf("queue closed, cannot prepare for acknowledgment")
	default:
		// Store in pending list with the ack ID
		pendingKey := q.getNackedItemKey()
		err := q.client.HSet(q.ctx, pendingKey, ackID, item).Err()
		if err != nil {
			return fmt.Errorf("error storing item in pending list: %v", err)
		}

		// Set the expiration for the pending entry
		if q.ackTimeout > 0 {
			q.client.Expire(q.ctx, pendingKey, q.ackTimeout)
		}

		return nil
	}
}

// Acknowledge removes an item from the pending list indicating successful processing
func (q *Queue) Acknowledge(ackID string) bool {
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot acknowledge")
		return false
	default:
		_, err := q.client.HDel(q.ctx, q.getNackedItemKey(), ackID).Result()
		if err != nil {
			log.Printf("Error acknowledging item: %v", err)
			return false
		}
		return true
	}
}

// requeueNackedItems checks for un-acknowledged items in the nacked list
// and returns them to the main queue to be processed again
func (q *Queue) requeueNackedItems() error {
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot requeue idle items")
		return fmt.Errorf("queue closed")
	default:
		// Get all pending items
		pendingItems, err := q.client.HGetAll(q.ctx, q.getNackedItemKey()).Result()
		if err != nil {
			log.Printf("Error getting pending items: %v", err)
			return fmt.Errorf("error getting pending items: %v", err)
		}

		for ackID, item := range pendingItems {
			// Move from pending back to the main queue at the front
			// Using LPush to maintain FIFO ordering and prioritize previously timed-out items
			pipe := q.client.Pipeline()
			pipe.HDel(q.ctx, q.getNackedItemKey(), ackID)
			pipe.LPush(q.ctx, q.queueKey, item)

			if _, err := pipe.Exec(q.ctx); err != nil {
				log.Printf("Error requeueing idle item: %v", err)
				continue
			}
		}

		return nil
	}
}

// GetNackedItemsCount returns the number of items in the nacked list
func (q *Queue) GetNackedItemsCount() int {
	q.mx.Lock()
	defer q.mx.Unlock()

	count, err := q.client.HLen(q.ctx, q.getNackedItemKey()).Result()
	if err != nil {
		return 0
	}
	return int(count)
}

// getNackedItemKey returns the Redis key for the nacked items list
func (q *Queue) getNackedItemKey() string {
	return q.queueKey + ":nacked"
}

func (q *Queue) Close() error {
	q.cancel() // Cancel context to stop notification listener
	return nil
}

func (q *Queue) Listen() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	if r := recover(); r != nil {
		q.Close()
		signal.Stop(sigChan)
		close(sigChan)
		panic("Redis queue listener terminated due to panic")
	}

	<-sigChan
}
