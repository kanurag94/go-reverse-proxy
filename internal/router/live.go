package router

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync/atomic"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/cache"
	"github.com/kanurag94/go-reverse-proxy/internal/config"
	"github.com/kanurag94/go-reverse-proxy/internal/hook"
	"github.com/kanurag94/go-reverse-proxy/internal/proxy"
	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

type Snapshot struct {
	listeners  map[string]listener
	transports []*http.Transport
}

type listener struct {
	address string
	tls     []config.Cert // loaded once at startup, so a change needs a restart
	bump    string        // the bump CA's dir, also loaded once at startup
	mux     *Mux
}

// Options are what an embedder adds to what the config says.
type Options struct {
	// Hooks returns the hooks to run on an upstream's exchanges, inside its
	// cache: they see only what reaches the upstream. Build asks once per
	// upstream, so a reload asks again. nil, or no hooks, means none.
	Hooks func(upstream string) []hook.Hook
}

// Build validates cfg and assembles its handlers, with one Transport per upstream.
// store holds cached responses; passing the same one to every Build keeps the
// cache across reloads.
func Build(cfg *config.Config, store *cache.Store, opts Options) (*Snapshot, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s := &Snapshot{listeners: make(map[string]listener)}
	handlers := make(map[string]http.Handler)
	for _, u := range cfg.Upstreams {
		timeouts := upstream.Timeouts{Connect: time.Duration(u.Timeouts.Connect), ResponseHeader: time.Duration(u.Timeouts.ResponseHeader)}
		tr, err := upstream.NewTransport(timeouts, u.CAFile())
		if err != nil {
			return nil, fmt.Errorf("upstream %q: tls: %w", u.Name, err)
		}
		s.transports = append(s.transports, tr)
		var eps []*upstream.Endpoint
		for _, raw := range u.Endpoints {
			ep, _ := url.Parse(raw) // validated
			eps = append(eps, upstream.NewEndpoint(ep, tr))
		}
		health := upstream.Health{MaxFailures: u.Health.MaxFailures, EjectDuration: time.Duration(u.Health.EjectDuration)}
		var h http.Handler = proxy.New(upstream.NewPool(eps, u.Policy, health, u.Retries.Attempts))
		if opts.Hooks != nil {
			h = hook.New(h, opts.Hooks(u.Name)...)
		}
		if u.CacheEnabled() {
			h = cache.New(h, store, u.Name, u.Cache.PrivateHeaders)
		}
		handlers[u.Name] = h
	}
	for _, l := range cfg.Listeners {
		var hosts []Host
		for _, h := range l.Hosts {
			var routes []Route
			for _, r := range h.Routes {
				routes = append(routes, Route{Prefix: r.Prefix, Handler: handlers[r.Upstream]})
			}
			hosts = append(hosts, Host{Domains: h.Domains, Routes: routes})
		}
		s.listeners[l.Name] = listener{address: l.Address, tls: l.TLS, bump: l.BumpDir(), mux: NewMux(hosts)}
	}
	return s, nil
}

type Live struct {
	current atomic.Pointer[Snapshot]
}

func NewLive(s *Snapshot) *Live {
	l := &Live{}
	l.current.Store(s)
	return l
}

func (l *Live) Handler(name string) (http.Handler, error) {
	if _, ok := l.current.Load().listeners[name]; !ok {
		return nil, fmt.Errorf("unknown listener %q", name)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.current.Load().listeners[name].mux.ServeHTTP(w, r)
	}), nil
}

// HasDomain reports whether listener's current config routes host. Bumping asks it
// before minting, so a reload that adds or removes a domain applies at once.
func (l *Live) HasDomain(listener, host string) bool {
	li, ok := l.current.Load().listeners[listener]
	return ok && li.mux.HasDomain(host)
}

func (l *Live) Swap(next *Snapshot) error {
	if !sameListeners(l.current.Load(), next) {
		return errors.New("listener changes need a restart")
	}
	l.current.Swap(next).closeIdle()
	return nil
}

// closeIdle closes the snapshot's idle upstream connections. Connections still
// serving a request are left alone; IdleConnTimeout closes them once they are done.
func (s *Snapshot) closeIdle() {
	for _, t := range s.transports {
		t.CloseIdleConnections()
	}
}

func sameListeners(a, b *Snapshot) bool {
	if len(a.listeners) != len(b.listeners) {
		return false
	}
	for name, la := range a.listeners {
		lb, ok := b.listeners[name]
		if !ok || lb.address != la.address || !slices.Equal(lb.tls, la.tls) || lb.bump != la.bump {
			return false
		}
	}
	return true
}
