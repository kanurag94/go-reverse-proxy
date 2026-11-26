package intercept

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/bump"
)

const bumped = "api.example.test" // the one name these listeners bump

// serve runs an http.Server, answering with the request's protocol, on an
// intercepting listener. It returns the server, its address, the roots a client
// needs to trust it, and Serve's result once it returns.
func serve(t *testing.T) (*http.Server, string, *x509.CertPool, <-chan error) {
	t.Helper()
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "bumped "+r.Proto)
	})}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(NewListener(ln, ca, func(n string) bool { return n == bumped })) }()
	t.Cleanup(func() { srv.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	return srv, ln.Addr().String(), roots, served
}

// get sends one HTTPS request to addr as sni, trusting roots.
func get(addr string, roots *x509.CertPool, sni string, h2 bool) (string, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
		TLSClientConfig:   &tls.Config{ServerName: sni, RootCAs: roots},
		ForceAttemptHTTP2: h2,
	}
	if !h2 {
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://" + sni + "/")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

func TestListenerBumpsConfiguredName(t *testing.T) {
	_, addr, roots, _ := serve(t)
	for h2, want := range map[bool]string{true: "bumped HTTP/2.0", false: "bumped HTTP/1.1"} {
		if got, err := get(addr, roots, bumped, h2); err != nil || got != want {
			t.Errorf("h2=%v: got %q, %v; want %q", h2, got, err, want)
		}
	}
}

// A client offering no ALPN at all speaks HTTP/1.1, and is bumped.
func TestListenerBumpsClientWithoutALPN(t *testing.T) {
	_, addr, roots, _ := serve(t)
	c, err := tls.Dial("tcp", addr, &tls.Config{ServerName: bumped, RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+bumped+"\r\nConnection: close\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, _ := io.ReadAll(c); !strings.HasSuffix(string(b), "bumped HTTP/1.1") {
		t.Errorf("response %q, want it served over HTTP/1.1", b)
	}
}

// Until passthrough exists (step 4), a connection that is not bumped is closed.
func TestListenerDoesNotBumpOtherNames(t *testing.T) {
	_, addr, roots, _ := serve(t)
	if _, err := get(addr, roots, "other.example.test", true); err == nil {
		t.Error("an unconfigured name was served")
	}
}

func TestListenerDoesNotBumpNonHTTPALPN(t *testing.T) {
	_, addr, roots, _ := serve(t)
	c, err := tls.Dial("tcp", addr, &tls.Config{ServerName: bumped, RootCAs: roots, NextProtos: []string{"acme-tls/1"}})
	if err == nil {
		c.Close()
		t.Error("a client speaking acme-tls/1 was bumped")
	}
}

func TestShutdownClosesTheListener(t *testing.T) {
	srv, addr, _, served := serve(t)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v, want ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve still running after Shutdown")
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Error("the real listener still accepts after Shutdown")
	}
}
