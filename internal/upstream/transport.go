package upstream

import (
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
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
// with the settings a proxy needs. It verifies endpoints against the roots in
// caFile, or the system's when caFile is "". With h2c it speaks HTTP/2 without
// TLS to http:// endpoints.
func NewTransport(t Timeouts, caFile string, h2c bool) (*http.Transport, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil               // never send upstream traffic through $HTTP_PROXY
	tr.DisableCompression = true // pass Accept-Encoding through as the client sent it
	tr.MaxIdleConnsPerHost = 100 // Go's default of 2 closes and reopens connections under load
	tr.DialContext = (&net.Dialer{
		Timeout:   cmp.Or(t.Connect, defaultConnectTimeout),
		KeepAlive: 30 * time.Second,
	}).DialContext
	tr.ResponseHeaderTimeout = cmp.Or(t.ResponseHeader, defaultResponseHeaderTimeout)
	if caFile != "" {
		roots, err := loadRoots(caFile)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: roots} // still verified; HTTP/2 stays on
	}
	if h2c {
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetUnencryptedHTTP2(true) // without HTTP1, http:// requests use HTTP/2 with prior knowledge
	}
	return tr, nil
}

// loadRoots reads a PEM file of certificates to trust.
func loadRoots(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("no certificates in %s", path)
	}
	return pool, nil
}
