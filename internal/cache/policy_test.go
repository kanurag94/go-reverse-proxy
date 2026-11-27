package cache

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func TestParseCacheControl(t *testing.T) {
	tests := []struct {
		lines []string
		want  map[string]string
	}{
		{[]string{"max-age=60"}, map[string]string{"max-age": "60"}},
		{[]string{"Max-Age=60, Public"}, map[string]string{"max-age": "60", "public": ""}},
		{[]string{`private="Set-Cookie", no-cache`}, map[string]string{"private": "Set-Cookie", "no-cache": ""}},
		{[]string{"no-store", "max-age=5"}, map[string]string{"no-store": "", "max-age": "5"}},
		{[]string{" , max-age=5,, "}, map[string]string{"max-age": "5"}},
	}
	for _, tt := range tests {
		if got := parseCacheControl(tt.lines); !maps.Equal(got, tt.want) {
			t.Errorf("parseCacheControl(%q) = %v, want %v", tt.lines, got, tt.want)
		}
	}

	if _, ok := parseCacheControl([]string{`no-cache="a, b"`})["no-cache"]; !ok {
		t.Error(`no-cache="a, b" lost no-cache; a quoted comma must not hide the directive`)
	}
}

func TestRequestPolicy(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		h          http.Header
		use, store bool
	}{
		{"plain GET", http.MethodGet, nil, true, true},
		{"HEAD", http.MethodHead, nil, true, false},
		{"POST", http.MethodPost, nil, false, false},
		{"range", http.MethodGet, hdr("Range", "bytes=0-9"), false, false},
		{"if-none-match", http.MethodGet, hdr("If-None-Match", `"v1"`), true, true},
		{"if-modified-since", http.MethodGet, hdr("If-Modified-Since", "Tue, 24 Nov 2026 10:00:00 GMT"), true, true},
		{"if-match", http.MethodGet, hdr("If-Match", `"v1"`), false, false},
		{"if-unmodified-since", http.MethodGet, hdr("If-Unmodified-Since", "Tue, 24 Nov 2026 10:00:00 GMT"), false, false},
		{"if-range", http.MethodGet, hdr("If-Range", `"v1"`), false, false},
		{"upgrade", http.MethodGet, hdr("Upgrade", "websocket"), false, false},
		{"no-store", http.MethodGet, hdr("Cache-Control", "no-store"), false, false},
		{"no-cache", http.MethodGet, hdr("Cache-Control", "no-cache"), false, true},
		{"max-age=0", http.MethodGet, hdr("Cache-Control", "max-age=0"), false, true},
		{"pragma no-cache", http.MethodGet, hdr("Pragma", "no-cache"), false, true},
		{"cache-control wins over pragma", http.MethodGet, hdr("Cache-Control", "max-age=60", "Pragma", "no-cache"), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "/", nil)
			if tt.h != nil {
				r.Header = tt.h
			}
			if use, store := requestPolicy(r); use != tt.use || store != tt.store {
				t.Errorf("use, store = %v, %v; want %v, %v", use, store, tt.use, tt.store)
			}
		})
	}
}

func TestFreshFor(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	date := now.Format(http.TimeFormat)
	in2m := now.Add(2 * time.Minute).Format(http.TimeFormat)
	auth := hdr("Authorization", "Bearer t")
	apiKey := hdr("X-Api-Key", "k")
	p := newPolicy([]string{"x-api-key"})

	tests := []struct {
		name   string
		req    http.Header
		status int
		res    http.Header
		want   time.Duration
	}{
		{"max-age", nil, 200, hdr("Cache-Control", "max-age=60"), 60 * time.Second},
		{"s-maxage wins", nil, 200, hdr("Cache-Control", "max-age=60, s-maxage=30"), 30 * time.Second},
		{"expires minus date", nil, 200, hdr("Expires", in2m, "Date", date), 2 * time.Minute},
		{"expires without date", nil, 200, hdr("Expires", in2m), 2 * time.Minute},
		{"max-age beats expires", nil, 200, hdr("Cache-Control", "max-age=10", "Expires", in2m), 10 * time.Second},
		{"age is subtracted", nil, 200, hdr("Cache-Control", "max-age=60", "Age", "20"), 40 * time.Second},
		{"already stale", nil, 200, hdr("Cache-Control", "max-age=60", "Age", "70"), 0},
		{"max-age=0", nil, 200, hdr("Cache-Control", "max-age=0"), 0},
		{"huge max-age is capped", nil, 200, hdr("Cache-Control", "max-age=99999999999"), 2147483648 * time.Second},
		{"invalid max-age", nil, 200, hdr("Cache-Control", "max-age=soon"), 0},
		{"negative max-age", nil, 200, hdr("Cache-Control", "max-age=-1"), 0},
		{"invalid expires", nil, 200, hdr("Expires", "0"), 0},
		{"no explicit lifetime", nil, 200, hdr("Last-Modified", date), 0},

		{"no-store", nil, 200, hdr("Cache-Control", "no-store, max-age=60"), 0},
		{"private", nil, 200, hdr("Cache-Control", "private, max-age=60"), 0},
		{"private with fields", nil, 200, hdr("Cache-Control", `private="X-User", max-age=60`), 0},
		{"no-cache", nil, 200, hdr("Cache-Control", "no-cache, max-age=60"), 0},
		{"set-cookie", nil, 200, hdr("Cache-Control", "max-age=60", "Set-Cookie", "id=1"), 0},
		{"vary star", nil, 200, hdr("Cache-Control", "max-age=60", "Vary", "*"), 0},
		{"vary header", nil, 200, hdr("Cache-Control", "max-age=60", "Vary", "Accept-Encoding"), 60 * time.Second},
		{"trailers", nil, 200, hdr("Cache-Control", "max-age=60", "Trailer", "X-Checksum"), 0},

		{"404 is cacheable", nil, 404, hdr("Cache-Control", "max-age=60"), 60 * time.Second},
		{"500 is not", nil, 500, hdr("Cache-Control", "max-age=60"), 0},
		{"206 is not", nil, 206, hdr("Cache-Control", "max-age=60"), 0},

		{"authorization", auth, 200, hdr("Cache-Control", "max-age=60"), 0},
		{"authorization + public", auth, 200, hdr("Cache-Control", "public, max-age=60"), 60 * time.Second},
		{"authorization + s-maxage", auth, 200, hdr("Cache-Control", "s-maxage=10"), 10 * time.Second},
		{"authorization + must-revalidate", auth, 200, hdr("Cache-Control", "must-revalidate, max-age=60"), 60 * time.Second},
		{"cookie", hdr("Cookie", "session=1"), 200, hdr("Cache-Control", "max-age=60"), 0},
		{"configured x-api-key", apiKey, 200, hdr("Cache-Control", "max-age=60"), 0},
		{"x-api-key + public", apiKey, 200, hdr("Cache-Control", "public, max-age=60"), 60 * time.Second},
		{"unconfigured header is no credential", hdr("X-Trace", "1"), 200, hdr("Cache-Control", "max-age=60"), 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			if req == nil {
				req = http.Header{}
			}
			if got := p.freshFor(req, tt.status, tt.res, now); got != tt.want {
				t.Errorf("freshFor = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInitialAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "30": 30 * time.Second, "soon": 0, "-5": 0} {
		if got := initialAge(hdr("Age", in)); got != want {
			t.Errorf("Age %q: got %v, want %v", in, got, want)
		}
	}
}
