package redisq

import (
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/redis/go-redis/v9"
)

type queues struct {
	client    *redis.Client
	namespace string

	wfStore atomic.Pointer[WorkflowStore] // the store built on this client, closed with it

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
	q := newQueue(qs.client, qs.key("fifo", queueKey), qs.scope())
	track(q)
	return q
}

func (qs *queues) NewPriorityQueue(queueKey string) *PriorityQueue {
	pq := newPriorityQueue(qs.client, qs.key("prio", queueKey), qs.scope())
	track(pq.Queue)
	return pq
}

// Close stops instance reports, removing this process's record, then
// closes the client.
func (qs *queues) Close() error {
	if s := qs.wfStore.Load(); s != nil {
		s.Close()
	}
	if qs.stopReports != nil {
		qs.stopReports()
		<-qs.reportsDone
	}
	return qs.client.Close()
}

// scope is the Redis address, database and key prefix: two clients share
// workflow state exactly when those match. Needs no store.
func (qs *queues) scope() string {
	o := qs.client.Options()
	return "redisq:" + o.Addr + "/" + strconv.Itoa(o.DB) + "/" + qs.prefix()
}

// liveQueues is every queue object of the process by scope and queue key,
// so a WorkflowStore finds the live object (its record retention) of a queue
// whichever client made it. Weak: an unreferenced queue drops out.
var liveQueues struct {
	mu sync.Mutex
	m  map[liveKey][]weak.Pointer[Queue]
}

type liveKey struct{ scope, key string }

// track adds q to liveQueues until it closes or is collected.
func track(q *Queue) {
	k := liveKey{q.scope, q.queueKey}
	w := weak.Make(q)
	liveQueues.mu.Lock()
	defer liveQueues.mu.Unlock()
	if liveQueues.m == nil {
		liveQueues.m = map[liveKey][]weak.Pointer[Queue]{}
	}
	liveQueues.m[k] = append(liveQueues.m[k], w)
	runtime.AddCleanup(q, func(k liveKey) { untrack(k, nil) }, k)
}

// untrack drops q's entry (nil: every dead one) for k.
func untrack(k liveKey, q *Queue) {
	liveQueues.mu.Lock()
	defer liveQueues.mu.Unlock()
	ws := slices.DeleteFunc(liveQueues.m[k], func(w weak.Pointer[Queue]) bool {
		v := w.Value()
		return v == nil || v == q
	})
	if len(ws) == 0 {
		delete(liveQueues.m, k)
	} else {
		liveQueues.m[k] = ws
	}
}

// liveQueue is the first live (open) object for scope and key, or nil.
func liveQueue(scope, key string) *Queue {
	liveQueues.mu.Lock()
	defer liveQueues.mu.Unlock()
	for _, w := range liveQueues.m[liveKey{scope, key}] {
		if q := w.Value(); q != nil && q.ctx.Err() == nil {
			return q
		}
	}
	return nil
}
