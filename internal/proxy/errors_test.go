package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/proxy"
	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

type failing struct{ err error }

func (f failing) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestUpstreamErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"refused", errors.New("connection refused"), http.StatusBadGateway},
		{"net timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, http.StatusGatewayTimeout},
		{"deadline", fmt.Errorf("upstream: %w", context.DeadlineExceeded), http.StatusGatewayTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			proxy.New(failing{tt.err}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestResponseHeaderTimeoutIs504(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer backend.Close()
	defer close(release)

	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}
	defer tr.CloseIdleConnections()

	rec := httptest.NewRecorder()
	proxy.New(upstream.NewEndpoint(u, tr)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", rec.Code)
	}
}

func TestEndedRequestStatus(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()

	tests := []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"client gone", cancelled, 499},
		{"caller deadline", expired, http.StatusGatewayTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(tt.ctx)
			rec := httptest.NewRecorder()
			proxy.New(failing{tt.ctx.Err()}).ServeHTTP(rec, in)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestErrorDetailGoesToLogNotClient(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	in := httptest.NewRequest(http.MethodGet, "/v1/chat?api_key=secret", nil)
	rec := httptest.NewRecorder()
	proxy.New(failing{errors.New("dial tcp 10.0.3.7:8080: connect: connection refused")}).ServeHTTP(rec, in)

	if body := rec.Body.String(); body != "Bad Gateway\n" {
		t.Errorf("client got %q, want only the status text", body)
	}
	log := buf.String()
	for _, want := range []string{"status=502", "path=/v1/chat ", "10.0.3.7:8080"} {
		if !strings.Contains(log, want) {
			t.Errorf("log %q lacks %q", log, want)
		}
	}
	if strings.Contains(log, "secret") {
		t.Errorf("log leaked the query: %q", log)
	}
}
