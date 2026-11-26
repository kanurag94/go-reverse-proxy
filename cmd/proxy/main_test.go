package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/cache"
	"github.com/kanurag94/go-reverse-proxy/internal/router"
	"github.com/kanurag94/go-reverse-proxy/pkg/config"
	"github.com/kanurag94/go-reverse-proxy/pkg/hook"
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

// lifetime counts a plugin's factories as the proxy creates and destroys them.
type lifetime struct{ created, destroyed atomic.Int32 }

func (l *lifetime) Create(string, []byte) (hook.FilterFactory, error) {
	l.created.Add(1)
	return lifetimeFactory{l}, nil
}

type lifetimeFactory struct{ l *lifetime }

func (lifetimeFactory) Create(hook.Handle) hook.Filter { return nil }
func (f lifetimeFactory) OnDestroy()                   { f.l.destroyed.Add(1) }

var plugins atomic.Int32

func TestShutdownDestroysPluginFactories(t *testing.T) {
	p := &lifetime{}
	name := fmt.Sprintf("lifetime-%d", plugins.Add(1)) // the registry outlives one run of a test
	hook.Register(name, p)
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"listeners": [{"name": "public", "address": "127.0.0.1:0", "hooks": [{"name": "` + name + `"}],
	  "hosts": [{"name": "api", "domains": ["api.local"], "routes": [{"prefix": "/", "upstream": "up"}]}]}],
	 "upstreams": [{"name": "up", "endpoints": ["` + backend(t, "a") + `"]}]}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, path) }()
	for deadline := time.Now().Add(5 * time.Second); p.created.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the plugin was never created")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the context ended")
	}
	if got := p.destroyed.Load(); got != 1 {
		t.Errorf("destroyed %d factories at shutdown, want 1", got)
	}
}
