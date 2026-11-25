package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/proxy"
	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

func proxyTo(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	return proxy.New(upstream.NewEndpoint(u, http.DefaultTransport))
}

func TestForwardsRequestAndResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Got-Method", r.Method)
		w.Header().Set("Got-Body", string(body))
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "created")
	}))
	defer backend.Close()
	front := httptest.NewServer(proxyTo(t, backend.URL))
	defer front.Close()

	res, err := http.Post(front.URL+"/items", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	if res.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", res.StatusCode)
	}
	if string(body) != "created" {
		t.Errorf("body = %q, want %q", body, "created")
	}
	if got := res.Header.Get("Got-Method"); got != http.MethodPost {
		t.Errorf("upstream saw method %q", got)
	}
	if got := res.Header.Get("Got-Body"); got != "hello" {
		t.Errorf("upstream saw body %q", got)
	}
	if got := res.Header.Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Errorf("Set-Cookie = %q, want both values in order", got)
	}
}

func TestDoesNotFollowRedirects(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer backend.Close()

	rec := httptest.NewRecorder()
	proxyTo(t, backend.URL).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/old", nil))

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/elsewhere" {
		t.Errorf("got %d Location %q, want 302 to /elsewhere", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUnreachableUpstreamIs502(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	backend.Close()

	rec := httptest.NewRecorder()
	proxyTo(t, backend.URL).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

type recorder struct{ got *http.Request }

func (rt *recorder) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.got = r
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
}

func TestClearsRequestURI(t *testing.T) {
	rt := &recorder{}
	in := httptest.NewRequest(http.MethodGet, "/a?x=1", nil)
	proxy.New(rt).ServeHTTP(httptest.NewRecorder(), in)

	if rt.got == nil || rt.got == in {
		t.Fatal("handler must send a clone of the request")
	}
	if rt.got.RequestURI != "" {
		t.Errorf("RequestURI = %q, want empty on an outgoing request", rt.got.RequestURI)
	}
	if in.RequestURI != "/a?x=1" {
		t.Errorf("caller's RequestURI changed to %q", in.RequestURI)
	}
}
