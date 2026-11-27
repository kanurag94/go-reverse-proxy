package cache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// versioned serves body with validators, and answers a request carrying
// them with 304 and notModified's headers.
func versioned(body string, notModified map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` || r.Header.Get("If-Modified-Since") == "Tue, 24 Nov 2026 10:00:00 GMT" {
			for k, v := range notModified {
				w.Header().Set(k, v)
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Tue, 24 Nov 2026 10:00:00 GMT")
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, body)
	}
}

func TestStaleEntryIsRevalidated(t *testing.T) {
	var sent http.Header
	h, clk, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Clone()
		versioned("hello", map[string]string{"Cache-Control": "max-age=30", "X-Version": "2"})(w, r)
	})
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)

	rec := do(h, http.MethodGet, "/a")
	if sent.Get("If-None-Match") != `"v1"` || sent.Get("If-Modified-Since") != "Tue, 24 Nov 2026 10:00:00 GMT" {
		t.Fatalf("revalidation sent If-None-Match %q, If-Modified-Since %q", sent.Get("If-None-Match"), sent.Get("If-Modified-Since"))
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" || cacheStatus(rec) != "go-reverse-proxy; fwd=stale; fwd-status=304" {
		t.Fatalf("got %d %q, Cache-Status %q; want the stored 200 hello, fwd-status=304", rec.Code, rec.Body, cacheStatus(rec))
	}
	if rec.Header().Get("X-Version") != "2" || rec.Header().Get("Content-Type") != "text/plain" || rec.Header().Get("Cache-Control") != "max-age=30" {
		t.Errorf("headers %v; want the stored ones updated by the 304's", rec.Header())
	}

	clk.add(20 * time.Second)
	hit := do(h, http.MethodGet, "/a")
	if calls.Load() != 2 || cacheStatus(hit) != "go-reverse-proxy; hit; ttl=10" || hit.Header().Get("X-Version") != "2" {
		t.Errorf("calls %d, Cache-Status %q, X-Version %q; want 2, a hit for the 304's 30s, 2",
			calls.Load(), cacheStatus(hit), hit.Header().Get("X-Version"))
	}
}

func TestRevalidationKeepsStoredContentLength(t *testing.T) {
	h, clk, _ := setup(t, versioned("hello", map[string]string{"Cache-Control": "max-age=30", "Content-Length": "0"}))
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)
	do(h, http.MethodGet, "/a")

	hit := do(h, http.MethodGet, "/a")
	if hit.Body.String() != "hello" || hit.Header().Get("Content-Length") == "0" {
		t.Errorf("body %q, Content-Length %q; want hello, not the 304's length", hit.Body, hit.Header().Get("Content-Length"))
	}
}

func TestChangedResourceReplacesStaleEntry(t *testing.T) {
	version := "1"
	h, clk, calls := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"`+version+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("ETag", `"`+version+`"`)
		io.WriteString(w, "v"+version)
	})
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)
	version = "2"

	rec := do(h, http.MethodGet, "/a")
	if rec.Body.String() != "v2" || rec.Header().Get("ETag") != `"2"` || cacheStatus(rec) != "go-reverse-proxy; fwd=stale" {
		t.Fatalf("got %q, ETag %q, Cache-Status %q; want the new v2 with its headers", rec.Body, rec.Header().Get("ETag"), cacheStatus(rec))
	}
	hit := do(h, http.MethodGet, "/a")
	if hit.Body.String() != "v2" || calls.Load() != 2 {
		t.Errorf("then %q after %d calls; want v2 from the store", hit.Body, calls.Load())
	}
}

func TestStaleWithoutValidatorsIsFetchedUnconditionally(t *testing.T) {
	var sent http.Header
	h, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { sent = r.Header.Clone(); cacheable("x")(w, r) })
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)
	do(h, http.MethodGet, "/a")

	if sent.Get("If-None-Match") != "" || sent.Get("If-Modified-Since") != "" {
		t.Errorf("sent %v; want no conditional headers", sent)
	}
}

func TestRevalidatedButNoLongerStorableIsDropped(t *testing.T) {
	h, clk, calls := setup(t, versioned("hello", map[string]string{"Cache-Control": "no-store"}))
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)

	rec := do(h, http.MethodGet, "/a")
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("got %d %q; want the stored response", rec.Code, rec.Body)
	}
	next := do(h, http.MethodGet, "/a")
	if calls.Load() != 3 || cacheStatus(next) != "go-reverse-proxy; fwd=uri-miss" {
		t.Errorf("calls %d, Cache-Status %q; want the entry gone", calls.Load(), cacheStatus(next))
	}
}

func TestRevalidationRestartsAge(t *testing.T) {
	h, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Age", "20")
	})
	do(h, http.MethodGet, "/a")
	clk.add(40 * time.Second)
	do(h, http.MethodGet, "/a")
	clk.add(10 * time.Second)

	hit := do(h, http.MethodGet, "/a")
	if hit.Header().Get("Age") != "10" || cacheStatus(hit) != "go-reverse-proxy; hit; ttl=50" {
		t.Errorf("Age %q, Cache-Status %q; want 10 and ttl=50, counted from the 304", hit.Header().Get("Age"), cacheStatus(hit))
	}
}

func TestNotModifiedNeverReachesTheClient(t *testing.T) {
	h, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusNotModified)
			http.NewResponseController(w).Flush()
			io.WriteString(w, "junk")
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, "hello")
	})
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" || rec.Flushed {
		t.Errorf("got %d %q, flushed %v; want the stored 200 hello, nothing of the 304", rec.Code, rec.Body, rec.Flushed)
	}
}

func TestRevalidationAsksOnlyOurQuestion(t *testing.T) {
	var sent http.Header
	h, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Clone()
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Last-Modified", "Tue, 24 Nov 2026 10:00:00 GMT")
		io.WriteString(w, "hello")
	})
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)
	do(h, http.MethodGet, "/a", "If-None-Match", `"x"`)

	if sent.Get("If-None-Match") != "" || sent.Get("If-Modified-Since") != "Tue, 24 Nov 2026 10:00:00 GMT" {
		t.Errorf("sent If-None-Match %q, If-Modified-Since %q; want only our own validator",
			sent.Get("If-None-Match"), sent.Get("If-Modified-Since"))
	}
}
