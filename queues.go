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

	wf            *workflowStore // shared by every queue (see workflow.go)
	wfResultLimit int            // WithWorkflowResultLimit; 0 = default
	wfRetention   time.Duration  // WithWorkflowRetention; 0 = default

	reportEvery time.Duration // WithInstanceReports; 0 = off
	stopReports func()
	reportsDone chan struct{}
}

// Option configures New. An invalid option makes New panic, as a bad URL does.
type Option func(*queues) error

// WithNamespace scopes every key and channel the queues use under ns, as
// redisq:ns:fifo:{key} and redisq:ns:prio:{key}.
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
	// Deprecated: use NewQueue; every queue is distributed since v1.6.0.
	NewDistributedQueue(queueKey string) *Queue
	// Deprecated: use NewPriorityQueue; every queue is distributed since v1.6.0.
	NewDistributedPriorityQueue(queueKey string) *PriorityQueue
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
	qs.wf = newWorkflowStore(qs)
	if qs.reportEvery > 0 {
		qs.startReports()
	}
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
	return qs.prefix() + kind + ":{" + queueKey + "}"
}

// prefix is redisq: plus the namespace and a colon, if any.
func (qs *queues) prefix() string {
	if qs.namespace == "" {
		return prefix
	}
	return prefix + qs.namespace + ":"
}

func (qs *queues) NewQueue(queueKey string) *Queue {
	return newQueue(qs.client, qs.key("fifo", queueKey), qs.wf)
}

func (qs *queues) NewPriorityQueue(queueKey string) *PriorityQueue {
	return newPriorityQueue(qs.client, qs.key("prio", queueKey), qs.wf)
}

// NewDistributedQueue is NewQueue.
//
// Deprecated: use NewQueue; every queue is distributed since v1.6.0.
func (qs *queues) NewDistributedQueue(queueKey string) *Queue {
	return qs.NewQueue(queueKey)
}

// NewDistributedPriorityQueue is NewPriorityQueue.
//
// Deprecated: use NewPriorityQueue; every queue is distributed since v1.6.0.
func (qs *queues) NewDistributedPriorityQueue(queueKey string) *PriorityQueue {
	return qs.NewPriorityQueue(queueKey)
}

// Close stops instance reports, removing this process's record, then
// closes the client.
func (qs *queues) Close() error {
	if qs.stopReports != nil {
		qs.stopReports()
		<-qs.reportsDone
	}
	return qs.client.Close()
}
