package redisq

import (
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type queues struct {
	client    *redis.Client
	namespace string
}

// Option configures New. An invalid option makes New panic, as a bad URL does.
type Option func(*queues) error

// WithNamespace scopes every key and channel the queues use under ns, as
// redisq:ns:fifo:{key} and redisq:ns:prio:{key} (see docs/adr/0002-redisq-key-prefix.md).
// An empty ns is the same as none: redisq:fifo:{key}.
func WithNamespace(ns string) Option {
	return func(qs *queues) error {
		if strings.ContainsAny(ns, "{}") {
			return fmt.Errorf("redisq: namespace %q must not contain '{' or '}'", ns)
		}
		qs.namespace = ns
		return nil
	}
}

type Queues interface {
	NewQueue(queueKey string) *Queue
	NewPriorityQueue(queueKey string) *PriorityQueue
	NewDistributedQueue(queueKey string) *DistributedQueue
	NewDistributedPriorityQueue(queueKey string) *DistributedPriorityQueue
	Close() error
}

func New(url string, options ...Option) Queues {
	qs := &queues{}
	for _, o := range options {
		if err := o(qs); err != nil {
			panic(err)
		}
	}

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

	qs.client = redis.NewClient(opts)
	return qs
}

// prefix starts every Redis name redisq creates, so SCAN redisq:* finds them all.
const prefix = "redisq:"

// key is the Redis name of queue queueKey of kind fifo or prio. The hashtag
// wraps only queueKey, so a queue and its sidecar keys share a cluster slot
// and FIFO and priority queues of one name never meet.
func (qs *queues) key(kind, queueKey string) string {
	if queueKey == "" || strings.ContainsAny(queueKey, "{}") { // {} would hash the whole name
		panic(fmt.Sprintf("redisq: queue key %q must be non-empty and free of '{' and '}'", queueKey))
	}
	ns := prefix
	if qs.namespace != "" {
		ns += qs.namespace + ":"
	}
	return ns + kind + ":{" + queueKey + "}"
}

func (qs *queues) NewQueue(queueKey string) *Queue {
	q := newQueue(qs.client, qs.key("fifo", queueKey))
	go q.drain()
	return q
}

func (qs *queues) NewPriorityQueue(queueKey string) *PriorityQueue {
	q := newPriorityQueue(qs.client, qs.key("prio", queueKey))
	go q.drain()
	return q
}

func (qs *queues) NewDistributedQueue(queueKey string) *DistributedQueue {
	return newDistributedQueue(qs.client, qs.key("fifo", queueKey))
}

func (qs *queues) NewDistributedPriorityQueue(queueKey string) *DistributedPriorityQueue {
	return newDistributedPriorityQueue(qs.client, qs.key("prio", queueKey))
}

func (qs *queues) Close() error {
	return qs.client.Close()
}
