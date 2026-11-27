package cache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// versions serves "v<n>" for the current version n with cacheControl. While
// version 2 is served, each response first waits for gate.
type versions struct {
	version      atomic.Int32
	gate         chan struct{}
	open         func()
	cacheControl string
}

func newVersions(t *testing.T, cacheControl string) *versions {
	v := &versions{gate: make(chan struct{}), cacheControl: cacheControl}
	v.open = sync.OnceFunc(func() { close(v.gate) })
	t.Cleanup(v.open)
	v.version.Store(1)
	return v
}

func (v *versions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := v.version.Load()
	if n == 2 {
		<-v.gate
	}
	if r.Context().Err() != nil {
		return // the client left; nothing worth storing reaches it
	}
	w.Header().Set("Cache-Control", v.cacheControl)
	fmt.Fprintf(w, "v%d", n)
}

// serveWithin serves r, failing the test if that takes longer than 2s: a stale
// response must not wait for the refresh.
func serveWithin(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, r); close(done) }()
	select {
	case <-done:
		return rec
	case <-time.After(2 * time.Second):
		t.Fatal("the client waited for the refresh")
		return nil
	}
}

func TestStaleWhileRevalidate(t *testing.T) {
	v := newVersions(t, "max-age=60, stale-while-revalidate=30")
	h, clk, calls := setup(t, v.ServeHTTP)
	do(h, http.MethodGet, "/a")
	clk.add(70 * time.Second)
	v.version.Store(2)

	for range 5 { // the refresh is held at the gate, yet nobody waits for it
		rec := serveWithin(t, h, httptest.NewRequest(http.MethodGet, "/a", nil))
		if rec.Body.String() != "v1" || cacheStatus(rec) != "go-reverse-proxy; hit; ttl=-10" {
			t.Fatalf("got %q, Cache-Status %q; want the stale v1 at once", rec.Body, cacheStatus(rec))
		}
	}
	eventually(t, "the background refresh to reach the upstream", func() bool { return calls.Load() == 2 })
	v.open()
	eventually(t, "the refreshed copy", func() bool { return do(h, http.MethodGet, "/a").Body.String() == "v2" })
	if calls.Load() != 2 {
		t.Errorf("upstream called %d times; want 2: one background refresh for every stale request", calls.Load())
	}
}

func TestStaleWhileRevalidateWindowEnds(t *testing.T) {
	h, clk, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60, stale-while-revalidate=30")
		io.WriteString(w, "hello")
	})
	do(h, http.MethodGet, "/a")
	clk.add(90 * time.Second)

	rec := do(h, http.MethodGet, "/a")
	if cacheStatus(rec) != "go-reverse-proxy; fwd=stale" || calls.Load() != 2 {
		t.Errorf("Cache-Status %q after %d calls; want a fetch the client waits for", cacheStatus(rec), calls.Load())
	}
}

func TestBackgroundRefreshOutlivesTheClient(t *testing.T) {
	v := newVersions(t, "max-age=60, stale-while-revalidate=30")
	h, clk, calls := setup(t, v.ServeHTTP)
	do(h, http.MethodGet, "/a")
	clk.add(70 * time.Second)
	v.version.Store(2)

	ctx, cancel := context.WithCancel(context.Background())
	serveWithin(t, h, httptest.NewRequest(http.MethodGet, "/a", nil).WithContext(ctx))
	eventually(t, "the background refresh to reach the upstream", func() bool { return calls.Load() == 2 })
	cancel()
	v.open()

	eventually(t, "the refreshed copy", func() bool { return do(h, http.MethodGet, "/a").Body.String() == "v2" })
	if calls.Load() != 2 {
		t.Errorf("upstream called %d times; want 2: the refresh the departed client started must finish", calls.Load())
	}
}

func TestBackgroundRefreshOfHeadFetchesGet(t *testing.T) {
	var methods sync.Map
	h, clk, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		methods.Store(r.Method, true)
		w.Header().Set("Cache-Control", "max-age=60, stale-while-revalidate=30")
		io.WriteString(w, "hello")
	})
	do(h, http.MethodGet, "/a")
	clk.add(70 * time.Second)
	methods.Clear()

	if rec := do(h, http.MethodHead, "/a"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD got %d %q; want the stale headers", rec.Code, rec.Body)
	}
	eventually(t, "the refreshed copy", func() bool { return cacheStatus(do(h, http.MethodGet, "/a")) == "go-reverse-proxy; hit; ttl=60" })
	if _, head := methods.Load(http.MethodHead); head || calls.Load() != 2 {
		t.Errorf("upstream saw HEAD %v after %d calls; want one GET, whose body a GET can use", head, calls.Load())
	}
}

// failing serves "v1" with cacheControl (and an ETag if tagged), then fails with code.
func failing(cacheControl string, tagged bool, code int) http.HandlerFunc {
	var calls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.WriteHeader(code)
			io.WriteString(w, "upstream says no")
			return
		}
		w.Header().Set("Cache-Control", cacheControl)
		if tagged {
			w.Header().Set("ETag", `"v1"`)
		}
		io.WriteString(w, "v1")
	}
}

func TestStaleIfError(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		for _, tagged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d tagged=%v", code, tagged), func(t *testing.T) {
				h, clk, _ := setup(t, failing("max-age=60, stale-if-error=30", tagged, code))
				do(h, http.MethodGet, "/a")
				clk.add(70 * time.Second)

				rec := do(h, http.MethodGet, "/a")
				want := fmt.Sprintf("go-reverse-proxy; fwd=stale; fwd-status=%d", code)
				if rec.Code != http.StatusOK || rec.Body.String() != "v1" || cacheStatus(rec) != want {
					t.Errorf("got %d %q, Cache-Status %q; want the stale 200 v1, %s", rec.Code, rec.Body, cacheStatus(rec), want)
				}
			})
		}
	}
}

func TestStaleIfErrorOnlyForServerErrors(t *testing.T) {
	h, clk, _ := setup(t, failing("max-age=60, stale-if-error=30", false, http.StatusNotFound))
	do(h, http.MethodGet, "/a")
	clk.add(70 * time.Second)

	if rec := do(h, http.MethodGet, "/a"); rec.Code != http.StatusNotFound {
		t.Errorf("got %d; want the upstream's 404: an answer, not a failure", rec.Code)
	}
}

func TestStaleIfErrorWindowEnds(t *testing.T) {
	h, clk, _ := setup(t, failing("max-age=60, stale-if-error=30", false, http.StatusServiceUnavailable))
	do(h, http.MethodGet, "/a")
	clk.add(90 * time.Second)

	if rec := do(h, http.MethodGet, "/a"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d; want the 503: the copy is too old to stand in", rec.Code)
	}
}

func TestRevalidateDirectivesForbidStale(t *testing.T) {
	for _, d := range []string{"must-revalidate", "proxy-revalidate", "s-maxage=60"} {
		t.Run(d, func(t *testing.T) {
			h, clk, _ := setup(t, failing("max-age=60, stale-while-revalidate=30, stale-if-error=30, "+d, false, http.StatusServiceUnavailable))
			do(h, http.MethodGet, "/a")
			clk.add(70 * time.Second)

			if rec := do(h, http.MethodGet, "/a"); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("got %d %q; want the 503: %s forbids serving it stale", rec.Code, rec.Body, d)
			}
		})
	}
}

func TestUpstreamAnswerToTheClientsQuestionIsPassedOn(t *testing.T) {
	h, clk, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"x"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=60, stale-if-error=30")
		io.WriteString(w, "v1")
	})
	do(h, http.MethodGet, "/a")
	clk.add(70 * time.Second)

	rec := do(h, http.MethodGet, "/a", "If-None-Match", `"x"`)
	next := do(h, http.MethodGet, "/a")
	if rec.Code != http.StatusNotModified || cacheStatus(next) != "go-reverse-proxy; fwd=stale" || calls.Load() != 3 {
		t.Errorf("got %d, then Cache-Status %q after %d calls; want the 304 passed on, our copy still stale",
			rec.Code, cacheStatus(next), calls.Load())
	}
}

func TestBackgroundRefreshEndsItsFlight(t *testing.T) {
	h, clk, calls := setup(t, cacheableFor("max-age=60, stale-while-revalidate=30"))
	do(h, http.MethodGet, "/a")
	clk.add(70 * time.Second)
	do(h, http.MethodGet, "/a")
	eventually(t, "the refreshed copy", func() bool { return calls.Load() == 2 && flightFor(h, key) == nil })
}

func TestUpstreamFailureWithoutStaleIfErrorIsPassedOn(t *testing.T) {
	h, clk, _ := setup(t, failing("max-age=60", true, http.StatusServiceUnavailable))
	do(h, http.MethodGet, "/a")
	clk.add(70 * time.Second)

	if rec := do(h, http.MethodGet, "/a"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d; want the 503: nothing allows the stale copy to cover for it", rec.Code)
	}
}

func cacheableFor(cacheControl string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", cacheControl)
		w.Write([]byte("hello"))
	}
}

func TestBackgroundRefreshSkipsAReplacedCopy(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var n atomic.Int32
	h, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) > 2 {
			<-block // a refresh that should not have started stays in flight
		}
		cacheableFor("max-age=60, stale-while-revalidate=30")(w, r)
	})
	do(h, http.MethodGet, "/a")
	old := h.store.get(key)
	clk.add(70 * time.Second)
	do(h, http.MethodGet, "/a", "Cache-Control", "no-cache") // stores a newer copy

	h.refreshInBackground(httptest.NewRequest(http.MethodGet, "/a", nil), key, old)
	if flightFor(h, key) != nil {
		t.Error("a refresh started for a copy that was already replaced")
	}
}
