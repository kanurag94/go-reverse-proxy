package upstream

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Endpoint is one place an upstream's requests go. Its URL gives the Host header
// and TLS name; addr, when set, is the address dialed instead of resolving it.
type Endpoint struct {
	url  *url.URL
	addr string
	base http.RoundTripper
}

func NewEndpoint(u *url.URL, base http.RoundTripper) *Endpoint {
	return &Endpoint{url: u, base: base}
}

func (e *Endpoint) RoundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.URL.Scheme = e.url.Scheme
	out.URL.Host = e.url.Host
	out.URL.Path = strings.TrimSuffix(e.url.Path, "/") + r.URL.Path
	out.URL.RawPath = strings.TrimSuffix(e.url.EscapedPath(), "/") + r.URL.EscapedPath()
	out.URL.RawQuery = joinQuery(e.url.RawQuery, r.URL.RawQuery)
	out.Host = ""
	return e.base.RoundTrip(out)
}

func joinQuery(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + "&" + b
}

// At returns a copy of e that connects to ip. It gets a transport of its own, so
// its connections are never shared with another address.
func (e *Endpoint) At(ip string) *Endpoint {
	tr := e.base.(*http.Transport).Clone()
	addr := net.JoinHostPort(ip, port(e.url))
	dial := tr.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dial(ctx, network, addr)
	}
	return &Endpoint{url: e.url, addr: addr, base: tr}
}

// CloseIdleConnections closes the idle connections of e's transport.
func (e *Endpoint) CloseIdleConnections() {
	if tr, ok := e.base.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (e *Endpoint) key() string { return e.url.Host + "@" + e.addr }

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
