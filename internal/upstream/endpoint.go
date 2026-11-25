package upstream

import (
	"net/http"
	"net/url"
	"strings"
)

type Endpoint struct {
	url  *url.URL
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
