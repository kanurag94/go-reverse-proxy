package router

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"

	"github.com/kanurag94/go-reverse-proxy/internal/cache"
	"github.com/kanurag94/go-reverse-proxy/internal/config"
	"github.com/kanurag94/go-reverse-proxy/internal/proxy"
	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

type Snapshot struct {
	listeners map[string]listener
}

type listener struct {
	address string
	mux     *Mux
}

// Build validates cfg and assembles its handlers. store holds cached responses;
// passing the same one to every Build keeps the cache across reloads.
func Build(cfg *config.Config, transport http.RoundTripper, store *cache.Store) (*Snapshot, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	handlers := make(map[string]http.Handler)
	for _, u := range cfg.Upstreams {
		ep, _ := url.Parse(u.Endpoints[0]) // validated; step 6 replaces the first endpoint with a pool
		var h http.Handler = proxy.New(upstream.NewEndpoint(ep, transport))
		if u.CacheEnabled() {
			h = cache.New(h, store, u.Name, u.Cache.PrivateHeaders)
		}
		handlers[u.Name] = h
	}
	s := &Snapshot{listeners: make(map[string]listener)}
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
	l.current.Store(next)
	return nil
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
