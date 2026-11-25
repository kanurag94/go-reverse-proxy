package router_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/cache"
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
	return mustBuildWith(t, cfg, cache.NewStore(1<<20, 1<<16))
}

func mustBuildWith(t *testing.T, cfg *config.Config, store *cache.Store) *router.Snapshot {
	t.Helper()
	s, err := router.Build(cfg, store)
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
	if _, err := router.Build(&config.Config{}, nil); err == nil {
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

func countingBackend(t *testing.T, calls *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, "cacheable")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestBuildCachesOnlyEnabledUpstreams(t *testing.T) {
	tests := []struct {
		name   string
		cache  *config.UpstreamCache
		cached bool
	}{
		{"enabled", &config.UpstreamCache{Enabled: true}, true},
		{"no cache block", nil, false},
		{"enabled false", &config.UpstreamCache{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			cfg := oneRoute(":8080", countingBackend(t, &calls))
			cfg.Upstreams[0].Cache = tt.cache
			h := mustHandler(t, router.NewLive(mustBuild(t, cfg)), "public")

			get(h, "/a")
			rec := get(h, "/a")

			wantCalls, wantStatus := int32(2), false
			if tt.cached {
				wantCalls, wantStatus = 1, true
			}
			if calls.Load() != wantCalls || rec.Body.String() != "cacheable" {
				t.Errorf("backend called %d times, body %q; want %d, cacheable", calls.Load(), rec.Body, wantCalls)
			}
			if got := rec.Header().Get("Cache-Status") != ""; got != wantStatus {
				t.Errorf("Cache-Status present = %v, want %v: the cache must wrap only enabled upstreams", got, wantStatus)
			}
		})
	}
}

func TestBuildPassesPrivateHeaders(t *testing.T) {
	var calls atomic.Int32
	cfg := oneRoute(":8080", countingBackend(t, &calls))
	cfg.Upstreams[0].Cache = &config.UpstreamCache{Enabled: true, PrivateHeaders: []string{"X-Api-Key"}}
	h := mustHandler(t, router.NewLive(mustBuild(t, cfg)), "public")

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/a", nil)
		req.Host = "api.example.com"
		req.Header.Set("X-Api-Key", "k")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if calls.Load() != 2 {
		t.Errorf("backend called %d times, want 2: a request with a configured private header must not be shared", calls.Load())
	}
}

func TestCacheSurvivesSwap(t *testing.T) {
	var calls atomic.Int32
	cfg := oneRoute(":8080", countingBackend(t, &calls))
	cfg.Upstreams[0].Cache = &config.UpstreamCache{Enabled: true}
	store := cache.NewStore(1<<20, 1<<16)
	live := router.NewLive(mustBuildWith(t, cfg, store))
	h := mustHandler(t, live, "public")

	get(h, "/a")
	if err := live.Swap(mustBuildWith(t, cfg, store)); err != nil {
		t.Fatal(err)
	}
	rec := get(h, "/a")
	if calls.Load() != 1 || !strings.Contains(rec.Header().Get("Cache-Status"), "; hit") {
		t.Errorf("after a reload: backend called %d times, Cache-Status %q; want 1, a hit",
			calls.Load(), rec.Header().Get("Cache-Status"))
	}
}

func TestBuildSpreadsAcrossEndpoints(t *testing.T) {
	cfg := oneRoute(":8080", backend(t, "a"))
	cfg.Upstreams[0].Endpoints = append(cfg.Upstreams[0].Endpoints, backend(t, "b"))
	h := mustHandler(t, router.NewLive(mustBuild(t, cfg)), "public")

	var got []string
	for range 4 {
		got = append(got, get(h, "/").Body.String())
	}
	if !slices.Equal(got, []string{"a", "b", "a", "b"}) {
		t.Errorf("responses came from %v, want a, b, a, b", got)
	}
}

func TestBuildAppliesUpstreamTimeouts(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		io.WriteString(w, "late")
	}))
	t.Cleanup(slow.Close)
	cfg := oneRoute(":8080", slow.URL)
	cfg.Upstreams[0].Timeouts.ResponseHeader = config.Duration(50 * time.Millisecond)
	h := mustHandler(t, router.NewLive(mustBuild(t, cfg)), "public")

	if rec := get(h, "/"); rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504 from the upstream's 50ms response_header timeout", rec.Code)
	}
}

func TestBuildAppliesUpstreamHealth(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cfg := oneRoute(":8080", dead.URL)
	cfg.Upstreams[0].Policy = "failover"
	cfg.Upstreams[0].Endpoints = append(cfg.Upstreams[0].Endpoints, backend(t, "backup"))
	cfg.Upstreams[0].Health.MaxFailures = 2
	cfg.Upstreams[0].Retries.Attempts = 1 // no retries: this test is about ejection alone
	h := mustHandler(t, router.NewLive(mustBuild(t, cfg)), "public")

	var got []int
	for range 3 {
		got = append(got, get(h, "/").Code)
	}
	if !slices.Equal(got, []int{502, 502, 200}) {
		t.Errorf("statuses %v, want 502, 502, 200: max_failures 2 ejects the dead primary on its second failure", got)
	}
}

func TestBuildRetriesOnAnotherEndpoint(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cfg := oneRoute(":8080", dead.URL)
	cfg.Upstreams[0].Endpoints = append(cfg.Upstreams[0].Endpoints, backend(t, "alive"))
	h := mustHandler(t, router.NewLive(mustBuild(t, cfg)), "public")

	for i := range 4 {
		if rec := get(h, "/"); rec.Code != http.StatusOK || rec.Body.String() != "alive" {
			t.Errorf("request %d: %d %q; want every request answered by the live endpoint", i, rec.Code, rec.Body)
		}
	}
}

func TestBuildRetriesCanBeTurnedOff(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cfg := oneRoute(":8080", dead.URL)
	cfg.Upstreams[0].Endpoints = append(cfg.Upstreams[0].Endpoints, backend(t, "alive"))
	cfg.Upstreams[0].Retries.Attempts = 1
	h := mustHandler(t, router.NewLive(mustBuild(t, cfg)), "public")

	if rec := get(h, "/"); rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: with attempts 1 the dead endpoint's failure reaches the client", rec.Code)
	}
}

func TestSwapClosesOldIdleConnections(t *testing.T) {
	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	cfg := oneRoute(":8080", srv.URL)
	live := router.NewLive(mustBuild(t, cfg))
	get(mustHandler(t, live, "public"), "/") // leaves an idle keep-alive connection in the first snapshot
	if err := live.Swap(mustBuild(t, cfg)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced snapshot's idle connection is still open")
	}
}
