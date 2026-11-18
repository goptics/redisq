package redisq

import "sync"

// group is a group commit: callers queue a value and wait; the caller that
// finds the group idle (the leader) runs run for up to max queued values
// (and maxGroupBytes of them, at least one),
// then hands the rest to a new goroutine, so a leader waits for one call
// only. No timer: a lone value runs at once, and values that come in while
// a call is in flight go together in the next. One call in flight per group.
type group[T any] struct {
	mu      sync.Mutex
	pending []*groupReq[T]
	busy    bool
}

// groupReq is one queued value of size bytes; run sets ok. done is nil
// for a leader's own value: the leader runs it itself.
type groupReq[T any] struct {
	v    T
	size int
	ok   bool
	done chan struct{}
}

// maxGroupBytes caps the bytes of one run's values.
const maxGroupBytes = 4 << 20

// do queues v, of size bytes, and returns its ok once a run took it.
func (g *group[T]) do(v T, size, max int, run func([]*groupReq[T])) bool {
	r := &groupReq[T]{v: v, size: size}
	g.mu.Lock()
	g.pending = append(g.pending, r)
	lead := !g.busy // idle: pending was empty, so r goes in the first run
	g.busy = true
	if !lead {
		r.done = make(chan struct{})
	}
	g.mu.Unlock()
	if lead {
		g.flush(max, run)
		return r.ok
	}
	<-r.done
	return r.ok
}

func (g *group[T]) flush(max int, run func([]*groupReq[T])) {
	g.mu.Lock()
	n, bytes := 0, 0
	for n < min(len(g.pending), max) && (n == 0 || bytes+g.pending[n].size <= maxGroupBytes) {
		bytes += g.pending[n].size
		n++
	}
	batch := make([]*groupReq[T], n)
	copy(batch, g.pending)
	rest := copy(g.pending, g.pending[n:])
	clear(g.pending[rest:])
	g.pending = g.pending[:rest]
	g.mu.Unlock()

	run(batch)
	for _, r := range batch {
		if r.done != nil {
			close(r.done)
		}
	}

	g.mu.Lock()
	more := len(g.pending) > 0
	g.busy = more
	g.mu.Unlock()
	if more {
		go g.flush(max, run)
	}
}
