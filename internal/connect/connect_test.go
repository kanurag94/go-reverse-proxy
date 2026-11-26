package connect_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/bump"
	"github.com/kanurag94/go-reverse-proxy/internal/connect"
)

const bumped = "api.example.test"

type proxy struct {
	addr  string
	roots *x509.CertPool
}

func start(t *testing.T) proxy {
	t.Helper()
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			io.WriteString(w, "plain")
			return
		}
		fmt.Fprintf(w, "%s auth=%q", r.Proto, r.Header.Get("Proxy-Authorization"))
	})
	srv := connect.NewServer(ca, func(n string) bool { return n == bumped }, h)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	return proxy{addr: ln.Addr().String(), roots: roots}
}

func through(t *testing.T, p proxy, url, launchID string, roots *x509.CertPool, h2 bool, header http.Header) string {
	t.Helper()
	proxyURL := &neturl.URL{Scheme: "http", Host: p.addr}
	if launchID != "" {
		proxyURL.User = neturl.UserPassword("atp", launchID)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: h2}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	maps.Copy(req.Header, header)
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func basic(id string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte("atp:"+id)) }

func TestBumpsConfiguredName(t *testing.T) {
	p := start(t)
	for h2, proto := range map[bool]string{true: "HTTP/2.0", false: "HTTP/1.1"} {
		want := fmt.Sprintf("%s auth=%q", proto, basic("launch-1"))
		if got := through(t, p, "https://"+bumped+"/", "launch-1", p.roots, h2, nil); got != want {
			t.Errorf("h2=%v: got %s, want %s", h2, got, want)
		}
	}
}

func TestTunnelCredentialCannotBeFaked(t *testing.T) {
	p := start(t)
	fake := http.Header{"Proxy-Authorization": {basic("someone-else")}}
	want := fmt.Sprintf("HTTP/2.0 auth=%q", basic("launch-1"))
	if got := through(t, p, "https://"+bumped+"/", "launch-1", p.roots, true, fake); got != want {
		t.Errorf("got %s, want the tunnel's credential: %s", got, want)
	}
}

func TestPassesThroughOtherHosts(t *testing.T) {
	p := start(t)
	real := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "real") }))
	defer real.Close()
	realRoots := x509.NewCertPool()
	realRoots.AddCert(real.Certificate())
	if got := through(t, p, real.URL+"/", "launch-1", realRoots, true, nil); got != "real" {
		t.Errorf("got %q, want the real server's answer", got)
	}
}

// eager sends the CONNECT request in the same write as the ClientHello, and
// reads past the proxy's response before the first read.
type eager struct {
	net.Conn
	request string
	sent    bool
	r       *bufio.Reader
}

func (c *eager) Write(b []byte) (int, error) {
	if c.sent {
		return c.Conn.Write(b)
	}
	c.sent = true
	if _, err := c.Conn.Write(append([]byte(c.request), b...)); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *eager) Read(b []byte) (int, error) {
	if c.r == nil {
		c.r = bufio.NewReader(c.Conn)
		res, err := http.ReadResponse(c.r, nil)
		if err != nil {
			return 0, err
		}
		if res.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("CONNECT: %s", res.Status)
		}
	}
	return c.r.Read(b)
}

func TestClientHelloSentWithTheConnectRequest(t *testing.T) {
	p := start(t)
	raw, err := net.Dial("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	c := &eager{Conn: raw, request: "CONNECT " + bumped + ":443 HTTP/1.1\r\nHost: " + bumped + ":443\r\n\r\n"}
	if err := tls.Client(c, &tls.Config{ServerName: bumped, RootCAs: p.roots}).Handshake(); err != nil {
		t.Fatalf("handshake through a pipelined CONNECT: %v", err)
	}
}

func TestPlainRequestsReachTheHandler(t *testing.T) {
	p := start(t)
	res, err := http.Get("http://" + p.addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if b, _ := io.ReadAll(res.Body); string(b) != "plain" {
		t.Errorf("got %q, want plain", b)
	}
}

func TestNoTunnelCredentialMeansNone(t *testing.T) {
	p := start(t)
	fake := http.Header{"Proxy-Authorization": {basic("someone-else")}}
	if got := through(t, p, "https://"+bumped+"/", "", p.roots, true, fake); got != `HTTP/2.0 auth=""` {
		t.Errorf("got %s, want no credential", got)
	}
}

func TestWithoutCANothingIsBumped(t *testing.T) {
	srv := connect.NewServer(nil, func(string) bool { return true }, http.NotFoundHandler())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())
	real := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "real") }))
	defer real.Close()
	realRoots := x509.NewCertPool()
	realRoots.AddCert(real.Certificate())
	tr := &http.Transport{
		Proxy:             http.ProxyURL(&neturl.URL{Scheme: "http", Host: ln.Addr().String()}),
		TLSClientConfig:   &tls.Config{ServerName: "example.com", RootCAs: realRoots}, // a name, so SNI is sent
		ForceAttemptHTTP2: true,
	}
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(real.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if b, _ := io.ReadAll(res.Body); string(b) != "real" {
		t.Errorf("got %q, want the real server's answer", b)
	}
}
