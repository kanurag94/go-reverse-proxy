package router

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kanurag94/go-reverse-proxy/pkg/config"
)

// Route sends paths under Prefix to Handler.
type Route struct {
	Prefix  string
	Methods []string             // empty: any method
	Headers []config.HeaderMatch // every one must match
	Handler http.Handler
}

// Host is the routes for a set of domains, tried in order.
type Host struct {
	Domains []string
	Routes  []Route
}

// Mux routes a request by its host, then the first route covering its path.
type Mux struct {
	hosts map[string][]Route
}

// NewMux routes hosts.
func NewMux(hosts []Host) *Mux {
	m := &Mux{hosts: make(map[string][]Route)}
	for _, h := range hosts {
		routes := slices.Clone(h.Routes)
		for _, d := range h.Domains {
			m.hosts[d] = routes
		}
	}
	return m
}

func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !validPath(r.URL) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	for _, rt := range m.hosts[hostname(r.Host)] {
		if rt.matches(r) {
			rt.Handler.ServeHTTP(w, r)
			return
		}
	}
	http.NotFound(w, r)
}

// HasDomain reports whether m routes host (any port and case ignored).
func (m *Mux) HasDomain(host string) bool {
	_, ok := m.hosts[hostname(host)]
	return ok
}

func (rt Route) matches(r *http.Request) bool {
	if !config.Covers(rt.Prefix, r.URL.Path) {
		return false
	}
	if len(rt.Methods) > 0 && !slices.Contains(rt.Methods, r.Method) {
		return false
	}
	for _, h := range rt.Headers {
		if !h.Matches(r.Header) {
			return false
		}
	}
	return true
}

func hostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func validPath(u *url.URL) bool {
	if u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return false
	}
	for seg := range strings.SplitSeq(u.Path, "/") {
		seg, _, _ = strings.Cut(seg, ";")
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
