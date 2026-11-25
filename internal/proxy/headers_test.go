package proxy_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/proxy"
)

func forward(t *testing.T, in *http.Request) *http.Request {
	t.Helper()
	rt := &recorder{}
	proxy.New(rt).ServeHTTP(httptest.NewRecorder(), in)
	if rt.got == nil {
		t.Fatal("request was not forwarded")
	}
	return rt.got
}

func TestRemovesHopByHopRequestHeaders(t *testing.T) {
	in := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range map[string]string{
		"Connection":          "keep-alive, X-Hop",
		"X-Hop":               "v",
		"Keep-Alive":          "300",
		"Proxy-Connection":    "keep-alive",
		"Proxy-Authorization": "Basic c2VjcmV0",
		"Te":                  "deflate",
		"Trailer":             "X-Checksum",
		"Upgrade":             "foo",
		"Authorization":       "Bearer t",
		"Accept":              "text/plain",
	} {
		in.Header.Set(k, v)
	}
	out := forward(t, in)

	for _, h := range []string{"Connection", "X-Hop", "Keep-Alive", "Proxy-Connection", "Proxy-Authorization", "Te", "Trailer", "Upgrade"} {
		if v, ok := out.Header[h]; ok {
			t.Errorf("%s forwarded as %q", h, v)
		}
	}
	for _, h := range []string{"Authorization", "Accept"} {
		if out.Header.Get(h) == "" {
			t.Errorf("%s was dropped", h)
		}
	}
	if in.Header.Get("Connection") == "" {
		t.Error("the caller's request headers were modified")
	}
}

func TestKeepsTETrailers(t *testing.T) {
	tests := []struct {
		te   []string
		want string
	}{
		{[]string{"trailers"}, "trailers"},
		{[]string{"Trailers, deflate"}, "trailers"},
		{[]string{"deflate", "trailers"}, "trailers"},
		{[]string{"deflate"}, ""},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.te, "|"), func(t *testing.T) {
			in := httptest.NewRequest(http.MethodGet, "/", nil)
			in.Header["Te"] = tt.te
			if got := forward(t, in).Header.Get("Te"); got != tt.want {
				t.Errorf("Te = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetsForwardedHeaders(t *testing.T) {
	in := httptest.NewRequest(http.MethodGet, "/", nil)
	in.Host = "api.example.com"
	in.Header.Set("X-Forwarded-Host", "evil.example")
	in.Header.Set("X-Forwarded-Proto", "https")
	out := forward(t, in)

	want := map[string]string{
		"X-Forwarded-For":   "192.0.2.1",
		"X-Forwarded-Host":  "api.example.com",
		"X-Forwarded-Proto": "http",
		"Via":               "1.1 go-reverse-proxy",
	}
	for h, v := range want {
		if got := out.Header.Values(h); len(got) != 1 || got[0] != v {
			t.Errorf("%s = %q, want [%q]", h, got, v)
		}
	}
}

func TestForwardedProtoHTTPS(t *testing.T) {
	in := httptest.NewRequest(http.MethodGet, "/", nil)
	in.TLS = &tls.ConnectionState{}
	if got := forward(t, in).Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", got)
	}
}

func TestAppendsXForwardedFor(t *testing.T) {
	tests := []struct {
		prior []string
		want  string
	}{
		{nil, "192.0.2.1"},
		{[]string{"203.0.113.7"}, "203.0.113.7, 192.0.2.1"},
		{[]string{"203.0.113.7", "198.51.100.2"}, "203.0.113.7, 198.51.100.2, 192.0.2.1"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			in := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.prior != nil {
				in.Header["X-Forwarded-For"] = tt.prior
			}
			if got := forward(t, in).Header["X-Forwarded-For"]; len(got) != 1 || got[0] != tt.want {
				t.Errorf("X-Forwarded-For = %q, want [%q]", got, tt.want)
			}
		})
	}
}

func TestAppendsVia(t *testing.T) {
	tests := []struct {
		major, minor int
		prior, want  string
	}{
		{1, 1, "", "1.1 go-reverse-proxy"},
		{1, 0, "1.1 fred", "1.1 fred, 1.0 go-reverse-proxy"},
		{2, 0, "", "2 go-reverse-proxy"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			in := httptest.NewRequest(http.MethodGet, "/", nil)
			in.ProtoMajor, in.ProtoMinor = tt.major, tt.minor
			if tt.prior != "" {
				in.Header.Set("Via", tt.prior)
			}
			if got := forward(t, in).Header.Get("Via"); got != tt.want {
				t.Errorf("Via = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConnectionCannotRemoveOurHeaders(t *testing.T) {
	in := httptest.NewRequest(http.MethodGet, "/", nil)
	in.Header.Set("Connection", "X-Forwarded-For, X-Forwarded-Host, Via")
	out := forward(t, in)

	if got := out.Header.Get("X-Forwarded-For"); got != "192.0.2.1" {
		t.Errorf("X-Forwarded-For = %q", got)
	}
	if got := out.Header.Get("X-Forwarded-Host"); got != "example.com" {
		t.Errorf("X-Forwarded-Host = %q", got)
	}
	if got := out.Header.Get("Via"); got == "" {
		t.Error("Via was removed")
	}
}

func TestDoesNotAddUserAgent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Got-User-Agent", r.Header.Get("User-Agent"))
	}))
	defer backend.Close()

	for _, ua := range []string{"", "curl/8.0"} {
		in := httptest.NewRequest(http.MethodGet, "/", nil)
		if ua != "" {
			in.Header.Set("User-Agent", ua)
		}
		rec := httptest.NewRecorder()
		proxyTo(t, backend.URL).ServeHTTP(rec, in)
		if got := rec.Header().Get("Got-User-Agent"); got != ua {
			t.Errorf("client sent %q, upstream saw %q", ua, got)
		}
	}
}

type fixed struct{ res *http.Response }

func (f fixed) RoundTrip(*http.Request) (*http.Response, error) { return f.res, nil }

func TestRemovesHopByHopResponseHeaders(t *testing.T) {
	res := &http.Response{
		StatusCode: http.StatusOK,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Body:       http.NoBody,
		Header: http.Header{
			"Connection":         {"X-Hop"},
			"X-Hop":              {"secret"},
			"Keep-Alive":         {"timeout=5"},
			"Proxy-Authenticate": {"Basic"},
			"Via":                {"1.0 origin-cache"},
			"Content-Type":       {"text/plain"},
		},
	}
	rec := httptest.NewRecorder()
	proxy.New(fixed{res}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	for _, h := range []string{"Connection", "X-Hop", "Keep-Alive", "Proxy-Authenticate"} {
		if v, ok := rec.Header()[h]; ok {
			t.Errorf("%s reached the client as %q", h, v)
		}
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Via"); got != "1.0 origin-cache, 1.1 go-reverse-proxy" {
		t.Errorf("Via = %q", got)
	}
}
