package proxy_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestStreamsBeforeUpstreamFinishes(t *testing.T) {
	tests := []struct{ contentType, length string }{
		{"text/event-stream", ""},
		{"text/event-stream; charset=utf-8", "18"},
		{"application/octet-stream", ""},
	}
	for _, tt := range tests {
		t.Run(tt.contentType+" "+tt.length, func(t *testing.T) {
			release := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				if tt.length != "" {
					w.Header().Set("Content-Length", tt.length)
				}
				io.WriteString(w, "data: 1\n\n")
				w.(http.Flusher).Flush()
				<-release
				io.WriteString(w, "data: 2\n\n")
			}))
			defer backend.Close()
			front := httptest.NewServer(proxyTo(t, backend.URL))
			defer front.Close()
			defer close(release)

			line, err := bufio.NewReader(get(t, front.URL).Body).ReadString('\n')
			if err != nil || line != "data: 1\n" {
				t.Fatalf("got %q, %v; want the first event while the upstream is still open", line, err)
			}
		})
	}
}

func TestFlushesStreamHeadersImmediately(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer backend.Close()
	front := httptest.NewServer(proxyTo(t, backend.URL))
	defer front.Close()
	defer close(release)

	if res := get(t, front.URL); res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

func TestForwardsResponseTrailers(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		io.WriteString(w, "body")
		w.Header().Set("X-Checksum", "abc")
		w.Header().Set(http.TrailerPrefix+"X-Late", "xyz")
	}))
	defer backend.Close()
	front := httptest.NewServer(proxyTo(t, backend.URL))
	defer front.Close()

	res := get(t, front.URL)
	if _, ok := res.Trailer["X-Checksum"]; !ok {
		t.Error("X-Checksum was not announced before the body")
	}
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "body" {
		t.Fatalf("body = %q, %v", body, err)
	}
	for k, want := range map[string]string{"X-Checksum": "abc", "X-Late": "xyz"} {
		if got := res.Trailer.Get(k); got != want {
			t.Errorf("trailer %s = %q, want %q", k, got, want)
		}
	}
}

func TestUpstreamFailureMidBodyIsNotACleanEnd(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer backend.Close()
	front := httptest.NewServer(proxyTo(t, backend.URL))
	defer front.Close()

	body, err := io.ReadAll(get(t, front.URL).Body)
	if err == nil {
		t.Fatalf("client read %q and a clean end; want an error", body)
	}
}
