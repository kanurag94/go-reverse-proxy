package router_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/config"
	"github.com/kanurag94/go-reverse-proxy/internal/router"
)

func backend(t *testing.T, name string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func oneRoute(address, endpoint string) *config.Config {
	return &config.Config{
		Listeners: []config.Listener{{
			Name:    "public",
			Address: address,
			Hosts: []config.Host{{
				Name:    "api",
				Domains: []string{"api.example.com"},
				Routes:  []config.Route{{Prefix: "/", Upstream: "up"}},
			}},
		}},
		Upstreams: []config.Upstream{{Name: "up", Endpoints: []string{endpoint}}},
	}
}

func mustBuild(t *testing.T, cfg *config.Config) *router.Snapshot {
	t.Helper()
	s, err := router.Build(cfg, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustHandler(t *testing.T, live *router.Live, listener string) http.Handler {
	t.Helper()
	h, err := live.Handler(listener)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "api.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBuildRoutesToUpstreams(t *testing.T) {
	cfg := oneRoute(":8080", backend(t, "a"))
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{Name: "b", Endpoints: []string{backend(t, "b")}})
	cfg.Listeners[0].Hosts[0].Routes = []config.Route{
		{Prefix: "/b", Upstream: "b"},
		{Prefix: "/", Upstream: "up"},
	}
	h := mustHandler(t, router.NewLive(mustBuild(t, cfg)), "public")

	if got := get(h, "/b/x").Body.String(); got != "b" {
		t.Errorf("/b/x went to %q, want b", got)
	}
	if got := get(h, "/other").Body.String(); got != "a" {
		t.Errorf("/other went to %q, want a", got)
	}
}

func TestBuildValidates(t *testing.T) {
	if _, err := router.Build(&config.Config{}, http.DefaultTransport); err == nil {
		t.Fatal("Build accepted a config with no listeners")
	}
}

func TestHandlerUnknownListener(t *testing.T) {
	live := router.NewLive(mustBuild(t, oneRoute(":8080", backend(t, "a"))))
	if _, err := live.Handler("nope"); err == nil {
		t.Fatal("Handler accepted an unknown listener")
	}
}

func TestSwapChangesRouting(t *testing.T) {
	live := router.NewLive(mustBuild(t, oneRoute(":8080", backend(t, "old"))))
	h := mustHandler(t, live, "public")

	if err := live.Swap(mustBuild(t, oneRoute(":8080", backend(t, "new")))); err != nil {
		t.Fatal(err)
	}
	if got := get(h, "/").Body.String(); got != "new" {
		t.Errorf("after swap went to %q, want new", got)
	}
}

func TestSwapRejectsListenerChanges(t *testing.T) {
	live := router.NewLive(mustBuild(t, oneRoute(":8080", backend(t, "old"))))
	h := mustHandler(t, live, "public")

	moved := mustBuild(t, oneRoute(":9090", backend(t, "new")))
	if err := live.Swap(moved); err == nil {
		t.Fatal("Swap accepted a changed listener address")
	}
	renamed := oneRoute(":8080", backend(t, "new"))
	renamed.Listeners[0].Name = "other"
	if err := live.Swap(mustBuild(t, renamed)); err == nil {
		t.Fatal("Swap accepted a renamed listener")
	}
	if got := get(h, "/").Body.String(); got != "old" {
		t.Errorf("a rejected swap changed routing: went to %q", got)
	}
}

func TestInFlightRequestKeepsItsSnapshot(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		io.WriteString(w, "old")
	}))
	defer slow.Close()
	live := router.NewLive(mustBuild(t, oneRoute(":8080", slow.URL)))
	h := mustHandler(t, live, "public")

	done := make(chan string)
	go func() { done <- get(h, "/").Body.String() }()
	<-arrived
	if err := live.Swap(mustBuild(t, oneRoute(":8080", backend(t, "new")))); err != nil {
		t.Fatal(err)
	}
	close(release)

	if got := <-done; got != "old" {
		t.Errorf("in-flight request got %q, want old", got)
	}
	if got := get(h, "/").Body.String(); got != "new" {
		t.Errorf("next request got %q, want new", got)
	}
}

func TestConcurrentRequestsDuringSwaps(t *testing.T) {
	a := mustBuild(t, oneRoute(":8080", backend(t, "a")))
	b := mustBuild(t, oneRoute(":8080", backend(t, "b")))
	live := router.NewLive(a)
	h := mustHandler(t, live, "public")

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				rec := get(h, "/")
				if body := rec.Body.String(); rec.Code != http.StatusOK || (body != "a" && body != "b") {
					t.Errorf("got %d %q", rec.Code, body)
					return
				}
			}
		})
	}
	for i := range 100 {
		next := a
		if i%2 == 1 {
			next = b
		}
		if err := live.Swap(next); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
