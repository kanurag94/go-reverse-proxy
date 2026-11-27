package cache

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// tagged serves "hello" with an ETag and a Last-Modified, ignoring conditionals.
func tagged(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "max-age=60")
	w.Header().Set("ETag", `"v1"`)
	w.Header().Set("Last-Modified", "Tue, 24 Nov 2026 10:00:00 GMT")
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, "hello")
}

func TestConditionalHit(t *testing.T) {
	tests := []struct {
		name string
		kv   []string
		want int
	}{
		{"same tag", []string{"If-None-Match", `"v1"`}, http.StatusNotModified},
		{"weak tag", []string{"If-None-Match", `W/"v1"`}, http.StatusNotModified},
		{"tag in a list", []string{"If-None-Match", `"v0", "v1"`}, http.StatusNotModified},
		{"any tag", []string{"If-None-Match", "*"}, http.StatusNotModified},
		{"other tag", []string{"If-None-Match", `"v0"`}, http.StatusOK},
		{"not modified since", []string{"If-Modified-Since", "Tue, 24 Nov 2026 10:00:00 GMT"}, http.StatusNotModified},
		{"modified since", []string{"If-Modified-Since", "Mon, 23 Nov 2026 10:00:00 GMT"}, http.StatusOK},
		{"unparsable date", []string{"If-Modified-Since", "yesterday"}, http.StatusOK},
		{"tag wins over date", []string{"If-None-Match", `"v0"`, "If-Modified-Since", "Tue, 24 Nov 2026 10:00:00 GMT"}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, calls := setup(t, tagged)
			do(h, http.MethodGet, "/a")

			rec := do(h, http.MethodGet, "/a", tt.kv...)
			if rec.Code != tt.want || calls.Load() != 1 || cacheStatus(rec) != "go-reverse-proxy; hit; ttl=60" {
				t.Fatalf("got %d after %d calls, Cache-Status %q; want %d from the store", rec.Code, calls.Load(), cacheStatus(rec), tt.want)
			}
			if tt.want == http.StatusOK && rec.Body.String() != "hello" {
				t.Errorf("body %q; want hello", rec.Body)
			}
		})
	}
}

func TestNotModifiedFromStoreCarriesOnlyCacheHeaders(t *testing.T) {
	h, _, _ := setup(t, tagged)
	do(h, http.MethodGet, "/a")

	rec := do(h, http.MethodGet, "/a", "If-None-Match", `"v1"`)
	if rec.Body.Len() != 0 || rec.Header().Get("ETag") != `"v1"` || rec.Header().Get("Cache-Control") != "max-age=60" ||
		rec.Header().Get("Age") != "0" || rec.Header().Get("Content-Type") != "" {
		t.Errorf("body %q, headers %v; want no body, the validators and caching headers, no Content-Type", rec.Body, rec.Header())
	}
}

func TestConditionalMissIsForwardedAsIs(t *testing.T) {
	var sent string
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Get("If-None-Match")
		versioned("hello", map[string]string{"ETag": `"v1"`})(w, r)
	})

	rec := do(h, http.MethodGet, "/a", "If-None-Match", `"v1"`)
	if sent != `"v1"` || rec.Code != http.StatusNotModified {
		t.Fatalf("sent If-None-Match %q, got %d; want the client's own question answered by the upstream", sent, rec.Code)
	}
	next := do(h, http.MethodGet, "/a")
	if cacheStatus(next) != "go-reverse-proxy; fwd=uri-miss" {
		t.Errorf("then Cache-Status %q; want a miss: a 304 is not stored", cacheStatus(next))
	}
}

func TestFullAnswerToConditionalMissIsStored(t *testing.T) {
	h, _, calls := setup(t, tagged)
	do(h, http.MethodGet, "/a", "If-None-Match", `"v0"`)

	hit := do(h, http.MethodGet, "/a")
	if hit.Body.String() != "hello" || calls.Load() != 1 {
		t.Errorf("got %q after %d calls; want hello from the store", hit.Body, calls.Load())
	}
}

func TestConditionalOnStaleEntry(t *testing.T) {
	tests := []struct {
		name, client string
		want         int
	}{
		{"client has it", `"v1"`, http.StatusNotModified},
		{"client has another", `"v0"`, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent string
			h, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				sent = r.Header.Get("If-None-Match")
				versioned("hello", map[string]string{"Cache-Control": "max-age=60"})(w, r)
			})
			do(h, http.MethodGet, "/a")
			clk.add(60 * time.Second)

			rec := do(h, http.MethodGet, "/a", "If-None-Match", tt.client)
			if sent != `"v1"` || rec.Code != tt.want || cacheStatus(rec) != "go-reverse-proxy; fwd=stale; fwd-status=304" {
				t.Errorf("sent %q, got %d, Cache-Status %q; want our validator upstream, then %d", sent, rec.Code, cacheStatus(rec), tt.want)
			}
		})
	}
}

func TestPreconditionsBypass(t *testing.T) {
	for _, kv := range [][]string{
		{"If-Match", `"v1"`},
		{"If-Unmodified-Since", "Tue, 24 Nov 2026 10:00:00 GMT"},
		{"If-Range", `"v1"`, "Range", "bytes=0-1"},
	} {
		h, _, calls := setup(t, tagged)
		do(h, http.MethodGet, "/a")
		rec := do(h, http.MethodGet, "/a", kv...)
		if calls.Load() != 2 || cacheStatus(rec) != "go-reverse-proxy; fwd=bypass" {
			t.Errorf("%s: calls %d, Cache-Status %q; want it forwarded", kv[0], calls.Load(), cacheStatus(rec))
		}
	}
}

func TestHeadServedFromStoredGet(t *testing.T) {
	h, _, calls := setup(t, tagged)
	do(h, http.MethodGet, "/a")

	rec := do(h, http.MethodHead, "/a")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("ETag") != `"v1"` || calls.Load() != 1 {
		t.Errorf("got %d %q, ETag %q after %d calls; want the stored headers, no body, no upstream call",
			rec.Code, rec.Body, rec.Header().Get("ETag"), calls.Load())
	}
}

func TestHeadMissIsNotStored(t *testing.T) {
	var methods []string
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { methods = append(methods, r.Method); tagged(w, r) })
	do(h, http.MethodHead, "/a")

	rec := do(h, http.MethodGet, "/a")
	if len(methods) != 2 || methods[0] != http.MethodHead || rec.Body.String() != "hello" || cacheStatus(rec) != "go-reverse-proxy; fwd=uri-miss" {
		t.Errorf("upstream saw %v, GET got %q, Cache-Status %q; want HEAD forwarded, then a GET miss with the body",
			methods, rec.Body, cacheStatus(rec))
	}
}

func TestHeadRevalidatesStaleEntry(t *testing.T) {
	h, clk, calls := setup(t, versioned("hello", map[string]string{"Cache-Control": "max-age=60"}))
	do(h, http.MethodGet, "/a")
	clk.add(60 * time.Second)

	head := do(h, http.MethodHead, "/a")
	get := do(h, http.MethodGet, "/a")
	if head.Code != http.StatusOK || head.Body.Len() != 0 || get.Body.String() != "hello" || calls.Load() != 2 || cacheStatus(get) != "go-reverse-proxy; hit; ttl=60" {
		t.Errorf("HEAD %d %q, GET %q after %d calls, Cache-Status %q; want HEAD to refresh the entry for GET",
			head.Code, head.Body, get.Body, calls.Load(), cacheStatus(get))
	}
}

func TestConditionalOnStoredErrorIsSentWhole(t *testing.T) {
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "gone")
	})
	do(h, http.MethodGet, "/a")

	rec := do(h, http.MethodGet, "/a", "If-None-Match", `"v1"`)
	if rec.Code != http.StatusNotFound || rec.Body.String() != "gone" {
		t.Errorf("got %d %q; want the stored 404: only a success can be not modified", rec.Code, rec.Body)
	}
}

func TestConditionalWithoutStoredValidators(t *testing.T) {
	for _, kv := range [][]string{
		{"If-None-Match", `"v1",`},
		{"If-Modified-Since", "Tue, 24 Nov 2026 10:00:00 GMT"},
	} {
		h, _, _ := setup(t, cacheable("hello"))
		do(h, http.MethodGet, "/a")
		if rec := do(h, http.MethodGet, "/a", kv...); rec.Code != http.StatusOK || rec.Body.String() != "hello" {
			t.Errorf("%s: got %d %q; want the whole response: nothing to compare with", kv[0], rec.Code, rec.Body)
		}
	}
}

func TestHeadNoCacheIsNotStored(t *testing.T) {
	h, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Cache-Control", "max-age=60")
			return
		}
		cacheable("hello")(w, r)
	})
	do(h, http.MethodHead, "/a", "Cache-Control", "no-cache")

	if rec := do(h, http.MethodGet, "/a"); rec.Body.String() != "hello" {
		t.Errorf("GET got %q; want hello, not the HEAD's empty body", rec.Body)
	}
}
