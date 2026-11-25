package router

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kanurag94/go-reverse-proxy/internal/config"
)

type Route struct {
	Prefix  string
	Handler http.Handler
}

type Host struct {
	Domains []string
	Routes  []Route
}

type Mux struct {
	hosts map[string][]Route
}

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
		if config.Covers(rt.Prefix, r.URL.Path) {
			rt.Handler.ServeHTTP(w, r)
			return
		}
	}
	http.NotFound(w, r)
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
