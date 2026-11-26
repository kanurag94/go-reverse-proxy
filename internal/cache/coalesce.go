package cache

import (
	"context"
	"sync"
	"sync/atomic"
)

// group lets concurrent misses for one key share a single upstream request.
type group struct {
	mu       sync.Mutex
	inflight map[string]*flight
}

// flight is one upstream request that other requests for the same key wait on.
type flight struct {
	done    chan struct{}
	once    sync.Once
	waiters atomic.Int32
}

// join returns the flight for key, and whether the caller leads it.
func (g *group) join(key string) (f *flight, leader bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.inflight[key]; ok {
		f.waiters.Add(1)
		return f, false
	}
	f = &flight{done: make(chan struct{})}
	g.inflight[key] = f
	return f, true
}

// release ends the flight and lets its waiters go. Safe to call more than once.
func (g *group) release(key string, f *flight) {
	f.once.Do(func() {
		g.mu.Lock()
		delete(g.inflight, key)
		g.mu.Unlock()
		close(f.done)
	})
}

// wait blocks until the flight ends or the waiter's own client leaves.
func (f *flight) wait(ctx context.Context) {
	select {
	case <-f.done:
	case <-ctx.Done():
	}
}
