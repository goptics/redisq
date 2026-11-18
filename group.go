package redisq

import "sync"

// group is a group commit: callers queue a value and wait; the caller that
// finds the group idle (the leader) runs run for up to max queued values,
// then hands the rest to a new goroutine, so a leader waits for one call
// only. No timer: a lone value runs at once, and values that come in while
// a call is in flight go together in the next. One call in flight per group.
type group[T any] struct {
	mu      sync.Mutex
	pending []*groupReq[T]
	busy    bool
}

// groupReq is one queued value; run sets ok.
type groupReq[T any] struct {
	v    T
	ok   bool
	done chan struct{}
}

// do queues v and returns its ok once a run took it.
func (g *group[T]) do(v T, max int, run func([]*groupReq[T])) bool {
	r := &groupReq[T]{v: v, done: make(chan struct{})}
	g.mu.Lock()
	g.pending = append(g.pending, r)
	lead := !g.busy
	g.busy = true
	g.mu.Unlock()
	if lead {
		g.flush(max, run)
	}
	<-r.done
	return r.ok
}

func (g *group[T]) flush(max int, run func([]*groupReq[T])) {
	g.mu.Lock()
	n := min(len(g.pending), max)
	batch := make([]*groupReq[T], n)
	copy(batch, g.pending)
	rest := copy(g.pending, g.pending[n:])
	clear(g.pending[rest:])
	g.pending = g.pending[:rest]
	g.mu.Unlock()

	run(batch)
	for _, r := range batch {
		close(r.done)
	}

	g.mu.Lock()
	more := len(g.pending) > 0
	g.busy = more
	g.mu.Unlock()
	if more {
		go g.flush(max, run)
	}
}
