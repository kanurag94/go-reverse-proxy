package upstream_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

type recorder struct{ got *http.Request }

func (rt *recorder) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.got = r
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
}

func roundTrip(t *testing.T, endpoint, target string) (in, out *http.Request) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	in = httptest.NewRequest(http.MethodGet, "http://client.example"+target, nil)
	res, err := upstream.NewEndpoint(u, rec).RoundTrip(in)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return in, rec.got
}

func TestEndpointRewrite(t *testing.T) {
	tests := []struct {
		endpoint, target, wantPath, wantQuery string
	}{
		{"http://up", "/a", "/a", ""},
		{"http://up/", "/a", "/a", ""},
		{"http://up/base", "/a", "/base/a", ""},
		{"http://up/base/", "/a", "/base/a", ""},
		{"http://up/base", "/", "/base/", ""},
		{"http://up", "/a%2Fb", "/a%2Fb", ""},
		{"http://up/b%2Fc", "/x%2Fy", "/b%2Fc/x%2Fy", ""},
		{"http://up?k=v", "/a", "/a", "k=v"},
		{"http://up", "/a?x=1", "/a", "x=1"},
		{"http://up?k=v", "/a?x=1&x=2", "/a", "k=v&x=1&x=2"},
		{"http://up", "/a?b=2&a=1", "/a", "b=2&a=1"},
		{"http://up", "/a?x;y", "/a", "x;y"},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint+" "+tt.target, func(t *testing.T) {
			_, out := roundTrip(t, tt.endpoint, tt.target)
			if got := out.URL.EscapedPath(); got != tt.wantPath {
				t.Errorf("path = %q, want %q", got, tt.wantPath)
			}
			if got := out.URL.RawQuery; got != tt.wantQuery {
				t.Errorf("query = %q, want %q", got, tt.wantQuery)
			}
		})
	}
}

func TestEndpointSetsTargetAndClearsHost(t *testing.T) {
	_, out := roundTrip(t, "https://up:8443/base", "/a")
	if out.URL.Scheme != "https" || out.URL.Host != "up:8443" {
		t.Errorf("url = %s, want https://up:8443/...", out.URL)
	}
	if out.Host != "" {
		t.Errorf("Host = %q, want empty so the endpoint's own host is sent", out.Host)
	}
}

func TestEndpointDoesNotModifyRequest(t *testing.T) {
	in, out := roundTrip(t, "https://up/base?k=v", "/a%2Fb?x=1")
	if in == out {
		t.Fatal("RoundTrip passed the caller's request through; it must clone")
	}
	if got := in.URL.String(); got != "http://client.example/a%2Fb?x=1" {
		t.Errorf("caller's URL changed to %q", got)
	}
	if in.Host != "client.example" {
		t.Errorf("caller's Host changed to %q", in.Host)
	}
}

func TestEndpointOverTheWire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Got-URI", r.RequestURI)
		w.Header().Set("Got-Host", r.Host)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/base")
	if err != nil {
		t.Fatal(err)
	}
	in := httptest.NewRequest(http.MethodGet, "http://client.example/a%2Fb?x=1", nil)
	res, err := upstream.NewEndpoint(u, srv.Client().Transport).RoundTrip(in)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if got := res.Header.Get("Got-URI"); got != "/base/a%2Fb?x=1" {
		t.Errorf("upstream saw %q, want /base/a%%2Fb?x=1", got)
	}
	if got := res.Header.Get("Got-Host"); got != u.Host {
		t.Errorf("upstream saw Host %q, want %q", got, u.Host)
	}
}
