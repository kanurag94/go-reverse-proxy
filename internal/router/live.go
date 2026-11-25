package router

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/cache"
	"github.com/kanurag94/go-reverse-proxy/internal/config"
	"github.com/kanurag94/go-reverse-proxy/internal/proxy"
	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

type Snapshot struct {
	listeners  map[string]listener
	transports []*http.Transport
}

type listener struct {
	address string
	mux     *Mux
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
		tr := upstream.NewTransport(upstream.Timeouts{
			Connect:        time.Duration(u.Timeouts.Connect),
			ResponseHeader: time.Duration(u.Timeouts.ResponseHeader),
		})
		s.transports = append(s.transports, tr)
		var eps []*upstream.Endpoint
		for _, raw := range u.Endpoints {
			ep, _ := url.Parse(raw) // validated
			eps = append(eps, upstream.NewEndpoint(ep, tr))
		}
		health := upstream.Health{MaxFailures: u.Health.MaxFailures, EjectDuration: time.Duration(u.Health.EjectDuration)}
		var h http.Handler = proxy.New(upstream.NewPool(eps, u.Policy, health, 1)) // 7b: attempts from config
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
		s.listeners[l.Name] = listener{address: l.Address, mux: NewMux(hosts)}
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
		if lb, ok := b.listeners[name]; !ok || lb.address != la.address {
			return false
		}
	}
	return true
}
