package cache

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T, next http.HandlerFunc) (*Handler, *clock, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	c := &clock{time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	h := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		next(w, r)
	}), NewStore(1<<20, 1<<10), "api", nil)
	h.now = c.now
	return h, c, &calls
}

func do(h http.Handler, method, target string, kv ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i < len(kv); i += 2 {
		r.Header.Set(kv[i], kv[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func cacheable(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, body)
	}
}

func cacheStatus(rec *httptest.ResponseRecorder) string {
	return strings.Join(rec.Result().Header.Values("Cache-Status"), " | ")
}

func TestMissThenHit(t *testing.T) {
	h, clk, calls := setup(t, cacheable("hello"))

	miss := do(h, http.MethodGet, "/a")
	if miss.Body.String() != "hello" || cacheStatus(miss) != "go-reverse-proxy; fwd=uri-miss" {
		t.Fatalf("miss: body %q, Cache-Status %q", miss.Body, cacheStatus(miss))
	}
	clk.add(10 * time.Second)
	hit := do(h, http.MethodGet, "/a")

	if calls.Load() != 1 {
		t.Fatalf("upstream called %d times, want 1", calls.Load())
	}
	if hit.Code != http.StatusOK || hit.Body.String() != "hello" || hit.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("hit: %d %q, Content-Type %q", hit.Code, hit.Body, hit.Header().Get("Content-Type"))
	}
	if got := hit.Header().Get("Age"); got != "10" {
		t.Errorf("Age = %q, want 10", got)
	}
	if got := cacheStatus(hit); got != "go-reverse-proxy; hit; ttl=50" {
		t.Errorf("Cache-Status = %q", got)
	}
}

func TestStaleEntryIsRefetched(t *testing.T) {
	h, clk, calls := setup(t, cacheable("hello"))
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)

	rec := do(h, http.MethodGet, "/a")
	if calls.Load() != 2 || cacheStatus(rec) != "go-reverse-proxy; fwd=stale" {
		t.Errorf("calls = %d, Cache-Status %q; want 2, fwd=stale", calls.Load(), cacheStatus(rec))
	}
}

func TestAgeIncludesUpstreamAge(t *testing.T) {
	h, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Age", "20")
	})
	do(h, http.MethodGet, "/a")
	clk.add(5 * time.Second)

	rec := do(h, http.MethodGet, "/a")
	if rec.Header().Get("Age") != "25" || cacheStatus(rec) != "go-reverse-proxy; hit; ttl=35" {
		t.Errorf("Age %q, Cache-Status %q; want 25, ttl=35", rec.Header().Get("Age"), cacheStatus(rec))
	}
}

func TestNotStored(t *testing.T) {
	tests := []struct {
		name string
		req  []string
		next http.HandlerFunc
	}{
		{"no lifetime", nil, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") }},
		{"server error", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"over object limit", nil, cacheable(strings.Repeat("x", 2000))},
		{"short body", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Content-Length", "10")
			io.WriteString(w, "short")
		}},
		{"credentials", []string{"Authorization", "Bearer t"}, cacheable("private")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, calls := setup(t, tt.next)
			do(h, http.MethodGet, "/a", tt.req...)
			do(h, http.MethodGet, "/a", tt.req...)
			if calls.Load() != 2 {
				t.Errorf("upstream called %d times, want 2 (nothing stored)", calls.Load())
			}
		})
	}
}

func TestOversizedResponseReachesClientWhole(t *testing.T) {
	h, _, _ := setup(t, cacheable(strings.Repeat("x", 2000)))
	if n := do(h, http.MethodGet, "/a").Body.Len(); n != 2000 {
		t.Errorf("client got %d bytes, want 2000", n)
	}
}

type brokenClient struct{ *httptest.ResponseRecorder }

func (brokenClient) Write([]byte) (int, error) { return 0, errors.New("client gone") }

func TestNotStoredWhenClientWriteFails(t *testing.T) {
	h, _, calls := setup(t, cacheable("hello"))
	h.ServeHTTP(brokenClient{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/a", nil))
	do(h, http.MethodGet, "/a")
	if calls.Load() != 2 {
		t.Errorf("upstream called %d times, want 2: a body the client did not get must not be stored", calls.Load())
	}
}

func TestNotStoredWhenUpstreamAborts(t *testing.T) {
	h, _, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, "partial")
		panic(http.ErrAbortHandler)
	})
	for range 2 {
		func() {
			defer func() {
				if v := recover(); v != http.ErrAbortHandler {
					t.Errorf("panic = %v, want ErrAbortHandler to propagate", v)
				}
			}()
			do(h, http.MethodGet, "/a")
		}()
	}
	if calls.Load() != 2 {
		t.Errorf("upstream called %d times, want 2: an aborted body must not be stored", calls.Load())
	}
}

func TestBypass(t *testing.T) {
	h, _, calls := setup(t, cacheable("hello"))
	rec := do(h, http.MethodPost, "/a")
	do(h, http.MethodGet, "/a")
	if cacheStatus(rec) != "go-reverse-proxy; fwd=bypass" || calls.Load() != 2 {
		t.Errorf("Cache-Status %q, calls %d; want fwd=bypass, 2 (POST must not fill the cache)", cacheStatus(rec), calls.Load())
	}
}

func TestClientNoCacheRefreshes(t *testing.T) {
	version := "v1"
	h, _, calls := setup(t, func(w http.ResponseWriter, r *http.Request) { cacheable(version)(w, r) })
	do(h, http.MethodGet, "/a")
	version = "v2"

	rec := do(h, http.MethodGet, "/a", "Cache-Control", "no-cache")
	if rec.Body.String() != "v2" || cacheStatus(rec) != "go-reverse-proxy; fwd=request" {
		t.Errorf("no-cache: body %q, Cache-Status %q", rec.Body, cacheStatus(rec))
	}
	if rec := do(h, http.MethodGet, "/a"); rec.Body.String() != "v2" || calls.Load() != 2 {
		t.Errorf("after refresh: body %q, calls %d; want v2 from cache", rec.Body, calls.Load())
	}
}

func TestKeyIncludesHostAndUpstream(t *testing.T) {
	store := NewStore(1<<20, 1<<10)
	var calls atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cacheable("x")(w, r)
	})
	api, web := New(next, store, "api", nil), New(next, store, "web", nil)

	do(api, http.MethodGet, "http://a.example/p")
	do(api, http.MethodGet, "http://b.example/p")
	do(web, http.MethodGet, "http://a.example/p")
	do(api, http.MethodGet, "http://a.example/p")
	if calls.Load() != 3 {
		t.Errorf("upstream called %d times, want 3 (two hosts, two upstreams, then one hit)", calls.Load())
	}
}

func TestHitHeadersAreCopies(t *testing.T) {
	h, _, _ := setup(t, cacheable("hello"))
	do(h, http.MethodGet, "/a")
	do(h, http.MethodGet, "/a").Header()["Content-Type"][0] = "evil"

	if got := do(h, http.MethodGet, "/a").Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q: a hit shared the stored header slice", got)
	}
}

func TestCacheStatusIsAppended(t *testing.T) {
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Status", "origin-cache; hit")
		cacheable("x")(w, r)
	})
	if got := cacheStatus(do(h, http.MethodGet, "/a")); got != "origin-cache; hit | go-reverse-proxy; fwd=uri-miss" {
		t.Errorf("miss Cache-Status = %q", got)
	}
	if got := cacheStatus(do(h, http.MethodGet, "/a")); got != "origin-cache; hit | go-reverse-proxy; hit; ttl=60" {
		t.Errorf("hit Cache-Status = %q", got)
	}
}

func TestFlushBeforeWriteIsCaptured(t *testing.T) {
	var err error
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		err = http.NewResponseController(w).Flush()
	})
	rec := do(h, http.MethodGet, "/a")
	if err != nil || !rec.Flushed {
		t.Fatalf("Flush through the cache: err %v, flushed %v", err, rec.Flushed)
	}
	if cacheStatus(rec) != "go-reverse-proxy; fwd=uri-miss" {
		t.Errorf("Cache-Status = %q: the flush bypassed the capture", cacheStatus(rec))
	}
}

func TestVary(t *testing.T) {
	h, _, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Accept-Encoding")
		cacheable("enc="+r.Header.Get("Accept-Encoding"))(w, r)
	})
	steps := []struct {
		encoding, body, status string
		calls                  int32
	}{
		{"gzip", "enc=gzip", "go-reverse-proxy; fwd=uri-miss", 1},
		{"gzip", "enc=gzip", "go-reverse-proxy; hit; ttl=60", 1},
		{"br", "enc=br", "go-reverse-proxy; fwd=vary-miss", 2},
		{"br", "enc=br", "go-reverse-proxy; hit; ttl=60", 2},
		{"", "enc=", "go-reverse-proxy; fwd=vary-miss", 3},
		{"", "enc=", "go-reverse-proxy; hit; ttl=60", 3},
	}
	for i, s := range steps {
		var kv []string
		if s.encoding != "" {
			kv = []string{"Accept-Encoding", s.encoding}
		}
		rec := do(h, http.MethodGet, "/a", kv...)
		if rec.Body.String() != s.body || cacheStatus(rec) != s.status || calls.Load() != s.calls {
			t.Errorf("step %d (%q): body %q, Cache-Status %q, calls %d; want %q, %q, %d",
				i, s.encoding, rec.Body, cacheStatus(rec), calls.Load(), s.body, s.status, s.calls)
		}
	}
}

func TestVaryNamesAreCaseInsensitive(t *testing.T) {
	h, _, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "accept-language")
		cacheable("x")(w, r)
	})
	do(h, http.MethodGet, "/a", "Accept-Language", "de")
	do(h, http.MethodGet, "/a", "Accept-Language", "de")
	rec := do(h, http.MethodGet, "/a", "Accept-Language", "en")
	if calls.Load() != 2 || cacheStatus(rec) != "go-reverse-proxy; fwd=vary-miss" {
		t.Errorf("calls %d, Cache-Status %q; want 2, vary-miss", calls.Load(), cacheStatus(rec))
	}
}

func TestUnsafeMethodsInvalidate(t *testing.T) {
	tests := []struct {
		method      string
		status      int
		invalidates bool
	}{
		{http.MethodPost, http.StatusOK, true},
		{http.MethodPut, http.StatusNoContent, true},
		{http.MethodPatch, http.StatusOK, true},
		{http.MethodDelete, http.StatusOK, true},
		{http.MethodPost, http.StatusSeeOther, true},
		{http.MethodPost, http.StatusInternalServerError, false},
		{http.MethodDelete, http.StatusNotFound, false},
		{http.MethodOptions, http.StatusOK, false},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+http.StatusText(tt.status), func(t *testing.T) {
			h, _, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					w.WriteHeader(tt.status)
					return
				}
				cacheable("x")(w, r)
			})
			do(h, http.MethodGet, "/a")
			do(h, tt.method, "/a")
			do(h, http.MethodGet, "/a")

			want := int32(2)
			if tt.invalidates {
				want = 3
			}
			if calls.Load() != want {
				t.Errorf("upstream called %d times, want %d", calls.Load(), want)
			}
		})
	}
}

func TestInvalidationIsPerURI(t *testing.T) {
	h, _, calls := setup(t, cacheable("x"))
	do(h, http.MethodGet, "/a")
	do(h, http.MethodGet, "/b")
	do(h, http.MethodPost, "/a")
	rec := do(h, http.MethodGet, "/b")
	if calls.Load() != 3 || cacheStatus(rec) != "go-reverse-proxy; hit; ttl=60" {
		t.Errorf("calls %d, Cache-Status %q; a POST to /a must not invalidate /b", calls.Load(), cacheStatus(rec))
	}
}

type deadlineWriter struct {
	*httptest.ResponseRecorder
	set bool
}

func (d *deadlineWriter) SetWriteDeadline(time.Time) error {
	d.set = true
	return nil
}

func TestCaptureUnwraps(t *testing.T) {
	var err error
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		err = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
	})
	dw := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(dw, httptest.NewRequest(http.MethodGet, "/a", nil))
	if err != nil || !dw.set {
		t.Errorf("SetWriteDeadline through the cache: err %v, reached writer %v", err, dw.set)
	}
}
