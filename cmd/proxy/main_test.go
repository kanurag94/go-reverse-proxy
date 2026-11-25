package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/cache"
	"github.com/kanurag94/go-reverse-proxy/internal/config"
	"github.com/kanurag94/go-reverse-proxy/internal/router"
)

func backend(t *testing.T, body string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

func configFor(endpoint string) string {
	return `{"listeners": [{"name": "public", "address": "127.0.0.1:8080",
	  "hosts": [{"name": "api", "domains": ["api.local"], "routes": [{"prefix": "/", "upstream": "up"}]}]}],
	 "upstreams": [{"name": "up", "endpoints": ["` + endpoint + `"]}]}`
}

func newLive(t *testing.T, content string) (*router.Live, *cache.Store) {
	cfg, err := config.Parse([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	store := cache.NewStore(cfg.CacheLimits())
	snap, err := router.Build(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	return router.NewLive(snap), store
}

func body(t *testing.T, live *router.Live) string {
	h, err := live.Handler("public")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://api.local/", nil))
	return rec.Body.String()
}

func TestReloadAppliesNewConfig(t *testing.T) {
	live, store := newLive(t, configFor(backend(t, "old")))
	if err := reload([]byte(configFor(backend(t, "new"))), store, live); err != nil {
		t.Fatal(err)
	}
	if got := body(t, live); got != "new" {
		t.Errorf("body = %q, want the new upstream", got)
	}
}

func TestReloadKeepsOldConfigOnError(t *testing.T) {
	live, store := newLive(t, configFor(backend(t, "old")))
	if err := reload([]byte(`{"listeners": [`), store, live); err == nil {
		t.Fatal("broken config was accepted")
	}
	if got := body(t, live); got != "old" {
		t.Errorf("body = %q, want the old upstream still serving", got)
	}
}

func TestShutdownFinishesInFlightRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)

	got := make(chan string, 1)
	go func() {
		res, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			got <- err.Error()
			return
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		got <- string(b)
	}()
	<-started
	done := make(chan error, 1)
	go func() { done <- shutdown([]*http.Server{srv}, 5*time.Second) }()
	for { // release the handler only once shutdown has stopped accepting connections
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			break
		}
		c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	close(release)

	if b := <-got; b != "done" {
		t.Errorf("in-flight request got %q, want it to finish", b)
	}
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
