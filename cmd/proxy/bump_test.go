package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/accesslog"
	"github.com/kanurag94/go-reverse-proxy/internal/config"
	"github.com/kanurag94/go-reverse-proxy/internal/router"
)

const name = "api.example.test"

// tlsBackend answers with the protocol it was reached over; h2 turns HTTP/2 on.
// caFile holds its certificate, for the proxy to trust.
func tlsBackend(t *testing.T, h2 bool) (url, caFile string) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = h2
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caFile = filepath.Join(t.TempDir(), "upstream.pem")
	writePEM(t, caFile, "CERTIFICATE", srv.Certificate().Raw)
	return srv.URL, caFile
}

// bumping serves a bumping listener for name in front of endpoint and returns its
// address and the pool a client needs to trust it.
func bumping(t *testing.T, endpoint, caFile string) (string, *x509.CertPool) {
	caDir := t.TempDir()
	l := config.Listener{Name: "intercept", Address: "127.0.0.1:0", Bump: &config.Bump{CADir: caDir},
		Hosts: []config.Host{{Name: "api", Domains: []string{name}, Routes: []config.Route{{Prefix: "/", Upstream: "api"}}}}}
	u := config.Upstream{Name: "api", Endpoints: []string{endpoint}}
	if caFile != "" {
		u.TLS = &config.UpstreamTLS{CAFile: caFile}
	}
	snap, err := router.Build(&config.Config{Listeners: []config.Listener{l}, Upstreams: []config.Upstream{u}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := router.NewLive(snap)
	h, _ := live.Handler(l.Name)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	il, err := intercepting(ln, l, live)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: accesslog.New(h)}
	go srv.Serve(il)
	t.Cleanup(func() { srv.Close() })
	pemBytes, _ := os.ReadFile(filepath.Join(caDir, "ca.pem"))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pemBytes)
	return ln.Addr().String(), roots
}

func call(t *testing.T, addr string, roots *x509.CertPool, h2 bool) (*http.Response, string) {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: &tls.Config{ServerName: name, RootCAs: roots}, ForceAttemptHTTP2: h2}
	if !h2 {
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/", nil)
	req.Host = name
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestBumpH2ClientToH1Upstream(t *testing.T) {
	url, caFile := tlsBackend(t, false)
	addr, roots := bumping(t, url, caFile)
	if res, body := call(t, addr, roots, true); res.Proto != "HTTP/2.0" || body != "HTTP/1.1" {
		t.Errorf("client %s, upstream %s; want HTTP/2.0 then HTTP/1.1", res.Proto, body)
	}
}

func TestBumpH1ClientToH2Upstream(t *testing.T) {
	url, caFile := tlsBackend(t, true)
	addr, roots := bumping(t, url, caFile)
	if res, body := call(t, addr, roots, false); res.Proto != "HTTP/1.1" || body != "HTTP/2.0" {
		t.Errorf("client %s, upstream %s; want HTTP/1.1 then HTTP/2.0", res.Proto, body)
	}
}

func TestBumpUntrustedUpstreamIs502(t *testing.T) {
	url, _ := tlsBackend(t, false)
	addr, roots := bumping(t, url, "") // the upstream's certificate is not trusted
	if res, _ := call(t, addr, roots, true); res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
}
