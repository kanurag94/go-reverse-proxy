package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const key = "api example.com/a" // what h.key gives for GET /a in these tests

func flightFor(h *Handler, key string) *flight {
	h.flights.mu.Lock()
	defer h.flights.mu.Unlock()
	return h.flights.inflight[key]
}

func waiters(h *Handler, key string) int32 {
	if f := flightFor(h, key); f != nil {
		return f.waiters.Load()
	}
	return 0
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitAll waits for every request to finish, failing instead of hanging if one is stuck.
func waitAll(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("requests still blocked after 5s")
	}
}

func TestCoalescesConcurrentMisses(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	h, clk, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		cacheable("hello")(w, r)
	})

	const n = 50
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { recs[i] = do(h, http.MethodGet, "/a") })
	}
	eventually(t, "49 waiters", func() bool { return waiters(h, key) == n-1 })
	clk.add(10 * time.Second) // the waiters wait 10s; it must show in their Age
	unblock()
	waitAll(t, &wg)

	if calls.Load() != 1 {
		t.Fatalf("upstream called %d times, want 1", calls.Load())
	}
	hits := 0
	for _, rec := range recs {
		if rec.Body.String() != "hello" {
			t.Errorf("body %q, want hello", rec.Body)
		}
		if cacheStatus(rec) == "go-reverse-proxy; hit; ttl=50" && rec.Header().Get("Age") == "10" {
			hits++
		}
	}
	if hits != n-1 {
		t.Errorf("%d hits with Age 10, want %d (everyone but the leader)", hits, n-1)
	}
}

func TestUnstorableReleasesWaitersEarly(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	h, _, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, "first")
		http.NewResponseController(w).Flush()
		<-release
	})

	const n = 10
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { do(h, http.MethodGet, "/a") })
	}
	eventually(t, "every request to reach the upstream while the leader still streams",
		func() bool { return calls.Load() == n })
	unblock()
	waitAll(t, &wg)
}

func TestLeaderAbortReleasesWaiters(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var first atomic.Bool
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			w.Header().Set("Cache-Control", "max-age=60")
			io.WriteString(w, "partial")
			<-release
			panic(http.ErrAbortHandler)
		}
		cacheable("hello")(w, r)
	})

	const n = 5
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			defer func() { recover() }() // the leader's request re-panics, as net/http expects
			recs[i] = do(h, http.MethodGet, "/a")
		})
	}
	eventually(t, "4 waiters", func() bool { return waiters(h, key) == n-1 })
	unblock()
	waitAll(t, &wg)

	served := 0
	for _, rec := range recs {
		if rec != nil && rec.Body.String() == "hello" {
			served++
		}
	}
	if served != n-1 {
		t.Errorf("%d waiters got a full response, want %d", served, n-1)
	}
}

func TestWaiterStopsWhenClientLeaves(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			cacheable("hello")(w, r)
		case <-r.Context().Done(): // like the real proxy: a cancelled request fails at once
		}
	})

	var wg sync.WaitGroup
	wg.Go(func() { do(h, http.MethodGet, "/a") })
	eventually(t, "a leader", func() bool { return flightFor(h, key) != nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/a", nil).WithContext(ctx))
		close(done)
	}()
	eventually(t, "1 waiter", func() bool { return waiters(h, key) == 1 })
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter still waiting after its client left")
	}
	unblock()
	waitAll(t, &wg)
}

func TestNoCacheRequestsDoNotWait(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	h, _, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") == "" {
			<-release
		}
		cacheable("hello")(w, r)
	})

	var wg sync.WaitGroup
	wg.Go(func() { do(h, http.MethodGet, "/a") })
	eventually(t, "a leader", func() bool { return flightFor(h, key) != nil })

	do(h, http.MethodGet, "/a", "Cache-Control", "no-cache")
	if calls.Load() != 2 {
		t.Errorf("upstream called %d times, want 2: a no-cache request must not wait on another's flight", calls.Load())
	}
	unblock()
	waitAll(t, &wg)
}
