package upstream

import (
	"cmp"
	"net"
	"net/http"
	"time"
)

// Timeouts bound one upstream's connections. Zero means the default.
type Timeouts struct {
	Connect        time.Duration
	ResponseHeader time.Duration
}

const (
	defaultConnectTimeout        = 5 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
)

// NewTransport returns the HTTP client for one upstream: Go's default Transport
// with the settings a proxy needs.
func NewTransport(t Timeouts) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil               // never send upstream traffic through $HTTP_PROXY
	tr.DisableCompression = true // pass Accept-Encoding through as the client sent it
	tr.MaxIdleConnsPerHost = 100 // Go's default of 2 closes and reopens connections under load
	tr.DialContext = (&net.Dialer{
		Timeout:   cmp.Or(t.Connect, defaultConnectTimeout),
		KeepAlive: 30 * time.Second,
	}).DialContext
	tr.ResponseHeaderTimeout = cmp.Or(t.ResponseHeader, defaultResponseHeaderTimeout)
	return tr
}
