package redisq

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
)

// Job outcome signal (spec/redis-storage.md 1.6.0, 6.13). finishScript
// publishes a finished job's contract.JobOutcome on <base>:done:<job id>.
// WatchOutcome subscribes that channel on the queue object's one pub/sub
// connection, first watcher of an id only, and reads the Job Record right
// after, and again each sweep, for a job that ended before the subscribe
// or whose message was lost (a reconnect).

var (
	_ contract.OutcomeWatcher        = (*Queue)(nil)
	_ contract.OutcomeWatcher        = (*PriorityQueue)(nil)
	_ contract.ResultAcknowledgeable = (*Queue)(nil)
	_ contract.ResultAcknowledgeable = (*PriorityQueue)(nil)
)

// maxCatchUp caps the watched ids one sweep reads.
const maxCatchUp = 500

type outcomeWatch struct {
	fn   func(contract.JobOutcome)
	done atomic.Bool // called or cancelled
}

// outcomeWatchers are a Queue's watchers by job id. Close cancels q.ctx
// before it drops them, so a watch that finds q.ctx live under mu is
// dropped by Close. subs brings the outcome channel subscriptions in line
// with byID (see flushSubs).
type outcomeWatchers struct {
	mu   sync.Mutex
	byID map[string]map[*outcomeWatch]struct{}
	subs subFlush
}

// subFlush coalesces outcome channel (un)subscribes and catch-up reads of
// overlapping watches: a group commit, as group. dirty: ids whose watch set
// gained its first or lost its last watcher; catch: watched ids to read
// once their subscribe is written; waiters: WatchOutcome calls that return
// once a flush wrote their subscribe. subscribed is the flusher's own (one
// flush at a time).
type subFlush struct {
	mu         sync.Mutex
	dirty      map[string]struct{}
	catch      []string
	waiters    []chan struct{}
	busy       bool
	subscribed map[string]struct{}
}

// donePrefix is the outcome channel prefix, <base>:done:.
func (q *Queue) donePrefix() string {
	return q.queueKey + ":done:"
}

// WatchOutcome calls fn once, from its own goroutine, when job id completes,
// fails or is cancelled in any process; a job whose record is still kept is reported
// even if it ended before the call. After fn the watch ends by itself, and
// the id's channel is unsubscribed once it has no watchers. cancel stops an
// early watch; fn is not called after cancel returns, except a call already
// running. An empty id, or a closed queue, watches nothing. Close drops
// watchers without calling fn, except a delivery already under way: its fn
// may still run after Close returns.
func (q *Queue) WatchOutcome(id string, fn func(contract.JobOutcome)) (cancel func()) {
	w := &outcomeWatch{fn: fn}
	o := &q.outcomes
	o.mu.Lock()
	if id == "" || q.ctx.Err() != nil {
		o.mu.Unlock()
		return func() {}
	}
	if o.byID == nil {
		o.byID = map[string]map[*outcomeWatch]struct{}{}
	}
	set := o.byID[id]
	first := set == nil
	if first {
		set = map[*outcomeWatch]struct{}{}
		o.byID[id] = set
	}
	set[w] = struct{}{}
	o.mu.Unlock()

	q.syncSub(id, first, true)

	return func() {
		if w.done.CompareAndSwap(false, true) {
			q.unwatch(id, w)
		}
	}
}

// unwatch drops w, and the id's subscription with its last watcher.
func (q *Queue) unwatch(id string, w *outcomeWatch) {
	o := &q.outcomes
	o.mu.Lock()
	set := o.byID[id]
	if set == nil {
		o.mu.Unlock()
		return
	}
	delete(set, w)
	last := len(set) == 0
	if last {
		delete(o.byID, id)
	}
	o.mu.Unlock()
	if last {
		q.syncSub(id, false, false)
	}
}

// syncSub queues id for a flush (see flushSubs): first, its watch set just
// changed between empty and not (dirty); watch, a WatchOutcome call, which
// also reads id's record after its subscribe and waits for the flush. Else
// the flush runs on its own goroutine.
func (q *Queue) syncSub(id string, dirty, watch bool) {
	s := &q.outcomes.subs
	var done chan struct{}
	s.mu.Lock()
	if s.dirty == nil {
		s.dirty = map[string]struct{}{}
	}
	// every call marks id: a second watcher must not return before the
	// first watcher's SUBSCRIBE is written; flushSubs skips subscribed ids
	s.dirty[id] = struct{}{}
	if watch {
		s.catch = append(s.catch, id)
		done = make(chan struct{})
		s.waiters = append(s.waiters, done)
	}
	lead := !s.busy
	s.busy = true
	s.mu.Unlock()
	switch {
	case lead && watch:
		q.flushSubs()
	case lead:
		go q.flushSubs()
	}
	if done != nil {
		<-done
	}
}

// flushSubs subscribes the dirty ids that have watchers and unsubscribes
// those that lost them, one SUBSCRIBE and one UNSUBSCRIBE (writes only, no
// reply awaited), ends the waits, then reads the records of the watched ids
// queued for it (one HMGET, own goroutine), for jobs that ended before the
// subscribe. Flushes run one at a time, in order, so a channel's last
// write matches whether it has watchers. More queued: a new goroutine.
func (q *Queue) flushSubs() {
	o, s := &q.outcomes, &q.outcomes.subs
	s.mu.Lock()
	dirty, catch, waiters := s.dirty, s.catch, s.waiters
	s.dirty, s.catch, s.waiters = nil, nil, nil
	s.mu.Unlock()

	var sub, unsub []string
	o.mu.Lock()
	if s.subscribed == nil {
		s.subscribed = map[string]struct{}{}
	}
	for id := range dirty {
		_, watched := o.byID[id]
		_, on := s.subscribed[id]
		switch {
		case watched && !on:
			s.subscribed[id] = struct{}{}
			sub = append(sub, q.donePrefix()+id)
		case !watched && on:
			delete(s.subscribed, id)
			unsub = append(unsub, q.donePrefix()+id)
		}
	}
	o.mu.Unlock()
	if len(sub) > 0 {
		if err := q.presence.Subscribe(q.ctx, sub...); err != nil && q.ctx.Err() == nil {
			log.Printf("Error subscribing to %d job outcome(s): %v", len(sub), err) // the sweep catches up
		}
	}
	if len(unsub) > 0 {
		if err := q.presence.Unsubscribe(q.ctx, unsub...); err != nil && q.ctx.Err() == nil {
			log.Printf("Error unsubscribing from %d job outcome(s): %v", len(unsub), err)
		}
	}
	for _, w := range waiters {
		close(w)
	}
	if len(catch) > 0 {
		go q.catchUp(catch)
	}

	s.mu.Lock()
	more := len(s.dirty) > 0 || len(s.catch) > 0 || len(s.waiters) > 0
	s.busy = more
	s.mu.Unlock()
	if more {
		go q.flushSubs()
	}
}

// deliver ends every watch of id with out: each fn runs once, on its own
// goroutine, so a slow fn holds up neither the pub/sub reader nor the sweep.
// The set leaves byID before the goroutines start, so a Close in between
// can't stop them: fn may run after Close returns.
func (q *Queue) deliver(id string, out contract.JobOutcome) {
	o := &q.outcomes
	o.mu.Lock()
	set := o.byID[id]
	if set != nil {
		delete(o.byID, id)
	}
	o.mu.Unlock()
	if set != nil {
		q.syncSub(id, false, false)
	}

	for w := range set {
		go func() {
			if w.done.CompareAndSwap(false, true) {
				w.fn(out)
			}
		}()
	}
}

// close drops every watcher without calling it.
func (o *outcomeWatchers) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, set := range o.byID {
		for w := range set {
			w.done.Store(true)
		}
	}
	o.byID = nil
}

// outcome handles a message on an outcome channel and reports whether it
// was one. A payload that isn't a final JobOutcome (completed, failed or
// cancelled) is dropped; the sweep reads the record instead.
func (q *Queue) outcome(msg *redis.Message) bool {
	id, ok := strings.CutPrefix(msg.Channel, q.donePrefix())
	if !ok {
		return false
	}
	var out contract.JobOutcome
	if json.Unmarshal([]byte(msg.Payload), &out) == nil && final(out.State) {
		out.ID = id
		q.deliver(id, out)
	}
	return true
}

// sweepOutcomes reads the records of up to maxCatchUp watched ids (map
// order, so a larger set rotates) and delivers the ended ones.
func (q *Queue) sweepOutcomes() {
	o := &q.outcomes
	o.mu.Lock()
	ids := make([]string, 0, min(len(o.byID), maxCatchUp))
	for id := range o.byID {
		if len(ids) == maxCatchUp {
			break
		}
		ids = append(ids, id)
	}
	o.mu.Unlock()

	if len(ids) > 0 {
		q.catchUp(ids)
	}
}

// catchUp delivers the ids whose Job Record says they ended.
func (q *Queue) catchUp(ids []string) {
	recs, err := q.client.HMGet(q.ctx, q.getJobsKey(), ids...).Result()
	if err != nil {
		if q.ctx.Err() == nil {
			log.Printf("Error reading job outcomes: %v", err)
		}
		return
	}
	for i, r := range recs {
		s, ok := r.(string)
		if !ok {
			continue
		}
		if out, ok := outcomeOf(ids[i], s); ok {
			q.deliver(ids[i], out)
		}
	}
}

// final reports whether s ends a job (spec 6.13, 6.14).
func final(s contract.JobState) bool {
	return s == contract.JobCompleted || s == contract.JobFailed || s == contract.JobCancelled
}

// outcomeOf turns Job Record rec of job id into its outcome: the state, the
// last error's text, the result as stored. A cancelled record without
// errors (dropped before it ran) gets error "cancelled", as its message.
func outcomeOf(id, rec string) (contract.JobOutcome, bool) {
	var r struct {
		State  contract.JobState   `json:"state"`
		Errors []contract.JobError `json:"errors"`
		Result json.RawMessage     `json:"result"`
	}
	if json.Unmarshal([]byte(rec), &r) != nil || !final(r.State) {
		return contract.JobOutcome{}, false
	}
	out := contract.JobOutcome{ID: id, State: r.State, Result: r.Result}
	if n := len(r.Errors); n > 0 {
		out.Error = r.Errors[n-1].Error
	} else if r.State == contract.JobCancelled {
		out.Error = "cancelled"
	}
	return out, true
}
