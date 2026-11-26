// Package router assembles a config into handlers and serves the current one,
// swapping in a new one atomically on reload.
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
	"github.com/kanurag94/go-reverse-proxy/internal/filterchain"
	"github.com/kanurag94/go-reverse-proxy/internal/proxy"
	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
	"github.com/kanurag94/go-reverse-proxy/pkg/config"
	"github.com/kanurag94/go-reverse-proxy/pkg/hook"
)

// Snapshot is one config, assembled: each listener's routing, and the transports
// whose idle connections close when it is replaced.
type Snapshot struct {
	listeners  map[string]listener
	transports []*http.Transport
	factories  []hook.FilterFactory
}

type listener struct {
	address string
	tls     []config.Cert // loaded once at startup, so a change needs a restart
	bump    string        // the bump CA's dir, also loaded once at startup
	connect bool
	mux     *Mux
	handler http.Handler // the mux behind the listener's plugin filters
}

// Build validates cfg and assembles its handlers, with one Transport per upstream.
// store holds cached responses; passing the same one to every Build keeps the
// cache across reloads.
func Build(cfg *config.Config, store *cache.Store) (*Snapshot, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s := &Snapshot{listeners: make(map[string]listener)}
	handlers := make(map[string]http.Handler)
	for _, u := range cfg.Upstreams {
		timeouts := upstream.Timeouts{Connect: time.Duration(u.Timeouts.Connect), ResponseHeader: time.Duration(u.Timeouts.ResponseHeader)}
		tr, err := upstream.NewTransport(timeouts, u.CAFile(), u.H2C)
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
				routes = append(routes, Route{Prefix: r.Prefix, Methods: r.Methods, Headers: r.Headers, Handler: handlers[r.Upstream]})
			}
			hosts = append(hosts, Host{Domains: h.Domains, Routes: routes})
		}
		factories, err := hookFactories(l)
		if err != nil {
			destroy(s.factories)
			return nil, fmt.Errorf("listener %q: %w", l.Name, err)
		}
		s.factories = append(s.factories, factories...)
		mux := NewMux(hosts)
		s.listeners[l.Name] = listener{address: l.Address, tls: l.TLS, bump: l.BumpDir(), connect: l.Connect, mux: mux,
			handler: filterchain.New(mux, factories...)}
	}
	return s, nil
}

// Live serves the current Snapshot. Requests finish on the snapshot they began on.
type Live struct {
	current atomic.Pointer[Snapshot]
}

// NewLive serves s until a Swap.
func NewLive(s *Snapshot) *Live {
	l := &Live{}
	l.current.Store(s)
	return l
}

// Handler serves listener name's requests with whatever snapshot is current.
func (l *Live) Handler(name string) (http.Handler, error) {
	if _, ok := l.current.Load().listeners[name]; !ok {
		return nil, fmt.Errorf("unknown listener %q", name)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.current.Load().listeners[name].handler.ServeHTTP(w, r)
	}), nil
}

// HasDomain reports whether listener's current config routes host. Bumping asks it
// before minting, so a reload that adds or removes a domain applies at once.
func (l *Live) HasDomain(listener, host string) bool {
	li, ok := l.current.Load().listeners[listener]
	return ok && li.mux.HasDomain(host)
}

// Swap makes next current, unless it changes what listeners read only at start.
func (l *Live) Swap(next *Snapshot) error {
	if !sameListeners(l.current.Load(), next) {
		return errors.New("listener changes need a restart")
	}
	old := l.current.Swap(next)
	old.closeIdle()
	destroy(old.factories)
	return nil
}

// closeIdle closes the snapshot's idle upstream connections. Connections still
// serving a request are left alone; IdleConnTimeout closes them once they are done.
// hookFactories creates the filter factories of l's plugins, in order. On an
// error it destroys the ones already created.
func hookFactories(l config.Listener) ([]hook.FilterFactory, error) {
	var created []hook.FilterFactory
	for i, h := range l.Hooks {
		f, err := createFactory(l.Name, h)
		if err != nil {
			destroy(created)
			return nil, fmt.Errorf("hook %d %q: %w", i, h.Name, err)
		}
		created = append(created, f)
	}
	return created, nil
}

func createFactory(listener string, h config.Hook) (hook.FilterFactory, error) {
	cf, ok := hook.Lookup(h.Name)
	if !ok {
		return nil, errors.New("not registered")
	}
	return cf.Create(listener, h.Config)
}

func destroy(factories []hook.FilterFactory) {
	for _, f := range factories {
		f.OnDestroy()
	}
}

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
		if !ok || lb.address != la.address || !slices.Equal(lb.tls, la.tls) || lb.bump != la.bump || lb.connect != la.connect {
			return false
		}
	}
	return true
}
