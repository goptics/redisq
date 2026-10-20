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

	"github.com/lucsky/cuid"
	"github.com/redis/go-redis/v9"
)

type Queue struct {
	client            *redis.Client
	queueKey          string
	mx                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	expiration        time.Duration
	visibilityTimeout time.Duration
}

func newQueue(c *redis.Client, queueKey string) *Queue {
	q := newQueueBase(c, queueKey)

	// requeue nacked items
	q.RequeueNackedItems()

	return q
}

func newQueueBase(c *redis.Client, queueKey string) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		client:            c,
		queueKey:          queueKey,
		ctx:               ctx,
		cancel:            cancel,
		visibilityTimeout: 5 * time.Minute,
	}
	return q
}

// SetExpiration sets the expiration time for the Queue
func (q *Queue) SetExpiration(expiration time.Duration) {
	q.mx.Lock()
	defer q.mx.Unlock()
	q.expiration = expiration
}

// SetAckTimeout sets how long a job can be processing before being requeued.
//
// Deprecated: use SetVisibilityTimeout, which this calls. It used to expire
// the whole pending hash, which deleted every unacknowledged job instead of
// requeueing it. A value <= 0 meant "no expiry" and is ignored: a zero
// visibility timeout would requeue every pending job, even ones in progress.
func (q *Queue) SetAckTimeout(ackTimeout time.Duration) {
	if ackTimeout > 0 {
		q.SetVisibilityTimeout(ackTimeout)
	}
}

// SetVisibilityTimeout sets the visibility timeout for jobs
// This controls how long a job remains invisible in the nacked queue before being candidates for requeuing
func (q *Queue) SetVisibilityTimeout(d time.Duration) {
	q.mx.Lock()
	defer q.mx.Unlock()
	q.visibilityTimeout = d
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

// PrepareForFutureAck adds an item to the pending list for acknowledgment tracking
// Returns an error if the operation fails
func (q *Queue) PrepareForFutureAck(ackID string, item any) error {
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		return fmt.Errorf("queue closed, cannot prepare for acknowledgment")
	default:
		// Store in pending list with the ack ID
		pendingKey := q.getNackedItemKey()
		pipe := q.client.Pipeline()

		pipe.HSet(q.ctx, pendingKey, ackID, item)

		// If visibility timeout is set, track it in ZSET
		if q.visibilityTimeout > 0 {
			timeoutKey := q.getTimeoutKey()
			score := float64(time.Now().Add(q.visibilityTimeout).Unix())
			pipe.ZAdd(q.ctx, timeoutKey, redis.Z{
				Score:  score,
				Member: ackID,
			})
		}

		_, err := pipe.Exec(q.ctx)
		if err != nil {
			return fmt.Errorf("error storing item in pending list: %v", err)
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
		pipe := q.client.Pipeline()
		pipe.HDel(q.ctx, q.getNackedItemKey(), ackID)
		pipe.ZRem(q.ctx, q.getTimeoutKey(), ackID)

		_, err := pipe.Exec(q.ctx)

		if err != nil {
			log.Printf("Error acknowledging item: %v", err)
			return false
		}
		return true
	}
}

// ackPending is the tail of the ack pop scripts: it stores the popped item v
// in the pending hash under ARGV[1], with the same bookkeeping as
// PrepareForFutureAck. ARGV[2] is the visibility deadline in Unix seconds
// (0: none).
const ackPending = `
redis.call('HSET', KEYS[2], ARGV[1], v)
if tonumber(ARGV[2]) > 0 then redis.call('ZADD', KEYS[3], ARGV[2], ARGV[1]) end
return v`

// Pop and pending run as one script, so an item is always in the queue or in
// the pending hash: a client that dies between two round trips can't lose it.
var (
	listAckPop = redis.NewScript(`local v = redis.call('LPOP', KEYS[1])
if not v then return false end` + ackPending)
	zsetAckPop = redis.NewScript(`local v = redis.call('ZPOPMIN', KEYS[1])[1]
if not v then return false end
if #v >= 17 and string.byte(v, 1) == 255 then v = string.sub(v, 18) end` + ackPending)
)

func (q *Queue) DequeueWithAckId() (any, bool, string) {
	return q.dequeueWithAck(listAckPop)
}

// dequeueWithAck pops the next item with pop and marks it pending under a new
// ack ID in the same atomic step.
func (q *Queue) dequeueWithAck(pop *redis.Script) (any, bool, string) {
	q.mx.Lock()
	defer q.mx.Unlock()

	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot dequeue")
		return nil, false, ""
	}

	var deadline int64
	if q.visibilityTimeout > 0 {
		deadline = time.Now().Add(q.visibilityTimeout).Unix()
	}

	ackID := cuid.New()
	keys := []string{q.queueKey, q.getNackedItemKey(), q.getTimeoutKey()}
	v, err := pop.Run(q.ctx, q.client, keys, ackID, deadline).Text()
	if err == redis.Nil {
		return nil, false, ""
	}

	if err != nil {
		log.Printf("Error dequeuing with ack: %v", err)
		return nil, false, ""
	}

	return []byte(v), true, ackID
}

// requeueNackedItems checks for un-acknowledged items in the nacked list
// and returns them to the main queue to be processed again
func (q *Queue) RequeueNackedItems() error {
	q.mx.Lock()
	defer q.mx.Unlock()

	select {
	case <-q.ctx.Done():
		log.Println("queue closed, cannot requeue idle items")
		return fmt.Errorf("queue closed")
	default:
		var pendingItems map[string]string
		var err error

		// If visibility timeout is used, only get expired items
		if q.visibilityTimeout > 0 {
			// Find items with score <= now
			now := float64(time.Now().Unix())
			vals, err := q.client.ZRangeByScore(q.ctx, q.getTimeoutKey(), &redis.ZRangeBy{
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

			// Retrieve the actual items for these keys
			// We have to HGet them. Since HMGet isn't ideal for unknown keys, let's just HMGet or loop.
			// HMGet is better.
			if len(vals) > 0 {
				// To use HMGet we need keys.
				// However, we need to map results back to IDs.
				// Let's just HGetAll to be safe for now or optimize later.
				// Actually, we can just loop over vals and fetch from Hash, but HGetAll is efficient enough if list isn't huge.
				// But wait, if we have millions of pending, HGetAll is bad.
				// Better approach: HMGet.
				// vals is []string of ackIDs.
				items, err := q.client.HMGet(q.ctx, q.getNackedItemKey(), vals...).Result()
				if err != nil {
					log.Printf("Error fetching values for expired items: %v", err)
					return err
				}

				pendingItems = make(map[string]string)
				for i, id := range vals {
					if items[i] != nil {
						// items[i] is interface{}, likely string or []byte
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
			pendingItems, err = q.client.HGetAll(q.ctx, q.getNackedItemKey()).Result()
			if err != nil {
				log.Printf("Error getting pending items: %v", err)
				return fmt.Errorf("error getting pending items: %v", err)
			}
		}

		for ackID, item := range pendingItems {
			// Move from pending back to the main queue at the front
			// Using LPush to maintain FIFO ordering and prioritize previously timed-out items
			pipe := q.client.Pipeline()
			pipe.HDel(q.ctx, q.getNackedItemKey(), ackID)
			if q.visibilityTimeout > 0 {
				pipe.ZRem(q.ctx, q.getTimeoutKey(), ackID)
			}
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

// getTimeoutKey returns the Redis key for the visibility timeout ZSET
func (q *Queue) getTimeoutKey() string {
	return q.queueKey + ":timeouts"
}

func (q *Queue) Close() error {
	q.cancel() // Cancel context to stop notification listener
	return nil
}

func (q *Queue) Remove(item any) bool {
	q.mx.Lock()
	defer q.mx.Unlock()

	_, err := q.client.LRem(q.ctx, q.queueKey, 1, item).Result()

	return err == nil
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
