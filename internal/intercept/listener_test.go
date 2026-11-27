package intercept

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/bump"
)

const bumped = "api.example.test" // the one name these listeners bump

type fixture struct {
	srv    *http.Server
	il     *Listener
	addr   string
	roots  *x509.CertPool
	served <-chan error // Serve's result, once it returns
}

// serve runs an http.Server, answering with the request's protocol, on an
// intercepting listener that bumps only `bumped`.
func serve(t *testing.T) fixture { return serveDialing(t, nil) }

// serveDialing is serve with passthrough dialing through dial (nil: the default).
func serveDialing(t *testing.T, dial dialFunc) fixture {
	t.Helper()
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bumps := func(n string) bool { return n == bumped }
	var il *Listener
	if dial == nil {
		il = NewListener(ln, ca, bumps)
	} else {
		il = newListener(ln, ca, bumps, dial)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "bumped "+r.Proto)
	})}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(il) }()
	t.Cleanup(func() { srv.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	return fixture{srv: srv, il: il, addr: ln.Addr().String(), roots: roots, served: served}
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
	f := serve(t)
	for h2, want := range map[bool]string{true: "bumped HTTP/2.0", false: "bumped HTTP/1.1"} {
		if got, err := get(f.addr, f.roots, bumped, h2); err != nil || got != want {
			t.Errorf("h2=%v: got %q, %v; want %q", h2, got, err, want)
		}
	}
}

// A client offering no ALPN at all speaks HTTP/1.1, and is bumped.
func TestListenerBumpsClientWithoutALPN(t *testing.T) {
	f := serve(t)
	c, err := tls.Dial("tcp", f.addr, &tls.Config{ServerName: bumped, RootCAs: f.roots})
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

func TestShutdownClosesTheListener(t *testing.T) {
	f := serve(t)
	if err := f.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v, want ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve still running after Shutdown")
	}
	if c, err := net.Dial("tcp", f.addr); err == nil {
		c.Close()
		t.Error("the real listener still accepts after Shutdown")
	}
}

// handle hands il the server end of a fresh TCP connection bound for dst, and
// returns the client end. "" means the very address the connection came in on.
func handle(t *testing.T, il *Listener, dst string) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) // left open, so a dial back to it connects
	if dst == "" {
		dst = ln.Addr().String()
	}
	go func() {
		if c, err := ln.Accept(); err == nil {
			il.Handle(c, dst)
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// realServer stands for the real destination: a TLS server whose certificate is for example.com.
func realServer(t *testing.T) (*httptest.Server, *x509.CertPool) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "real") }))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return srv, roots
}

// over sends one HTTPS request as sni on conn, trusting roots.
func over(t *testing.T, conn net.Conn, roots *x509.CertPool, sni string) string {
	t.Helper()
	tr := &http.Transport{
		DialContext:     func(context.Context, string, string) (net.Conn, error) { return conn, nil },
		TLSClientConfig: &tls.Config{ServerName: sni, RootCAs: roots},
	}
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://" + sni + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestPassesThroughOtherNames(t *testing.T) {
	f := serve(t)
	real, realRoots := realServer(t)
	// trusting only the real server's certificate proves the TLS was not touched
	if got := over(t, handle(t, f.il, real.Listener.Addr().String()), realRoots, "example.com"); got != "real" {
		t.Errorf("got %q, want the real server's answer", got)
	}
}

func TestPassesThroughNonHTTPALPN(t *testing.T) {
	f := serve(t)
	real, _ := realServer(t)
	acme, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: real.TLS.Certificates, NextProtos: []string{"acme-tls/1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer acme.Close()
	go func() {
		if c, err := acme.Accept(); err == nil {
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	c := tls.Client(handle(t, f.il, acme.Addr().String()), &tls.Config{ServerName: bumped, NextProtos: []string{"acme-tls/1"}, InsecureSkipVerify: true})
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	if st := c.ConnectionState(); st.NegotiatedProtocol != "acme-tls/1" || !st.PeerCertificates[0].Equal(real.Certificate()) {
		t.Errorf("reached %q, %s; want the real server even for a bumped name", st.NegotiatedProtocol, st.PeerCertificates[0].Subject)
	}
}

func TestPassesThroughNonTLS(t *testing.T) {
	f := serve(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		if c, err := echo.Accept(); err == nil {
			io.Copy(c, c)
			c.Close()
		}
	}()
	c := handle(t, f.il, echo.Addr().String())
	io.WriteString(c, "GET / HTTP/1.0\r\n\r\n")
	if got := readN(t, c, 18); got != "GET / HTTP/1.0\r\n\r\n" {
		t.Errorf("echoed %q, want the first bytes intact", got)
	}
}

func TestRefusesToDialItself(t *testing.T) {
	f := serve(t)
	c := handle(t, f.il, "") // bound for the address it came in on
	tls.Client(c, &tls.Config{ServerName: "other.example.test"}).Handshake()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read: %v; want the connection closed, not tunnelled to itself", err)
	}
}

func TestDirectDestinationIsSNIOnListenerPort(t *testing.T) {
	dialed := make(chan string, 1)
	f := serveDialing(t, func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed <- addr
		return nil, errors.New("not dialing in a test")
	})
	go get(f.addr, f.roots, "other.example.test", true)
	_, port, _ := net.SplitHostPort(f.addr)
	select {
	case got := <-dialed:
		if want := net.JoinHostPort("other.example.test", port); got != want {
			t.Errorf("dialed %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never dialed")
	}
}

// syncBuffer is a log destination safe to write from many goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *syncBuffer {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func eventually(t *testing.T, ok func() bool, want string, log *syncBuffer) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %s:\n%s", want, log)
		}
	}
}

func TestLogsEachConnection(t *testing.T) {
	log := captureLog(t)
	f := serve(t)
	real, realRoots := realServer(t)
	c := handle(t, f.il, real.Listener.Addr().String())
	over(t, c, realRoots, "example.com")
	c.Close()
	get(f.addr, f.roots, bumped, true)
	counts := regexp.MustCompile(`sent=[1-9].* received=[1-9]`)
	eventually(t, func() bool {
		s := log.String()
		return strings.Contains(s, "mode=passthrough") && strings.Contains(s, "destination="+real.Listener.Addr().String()) &&
			counts.MatchString(s) && strings.Contains(s, "mode=bump") && strings.Contains(s, "sni="+bumped)
	}, "a passthrough line with destination and byte counts, and a bump line with the sni", log)
}

func TestListenerWithoutSocket(t *testing.T) {
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	il := New(ca, func(n string) bool { return n == bumped })
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "bumped") })}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(il) }()
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	server, client := net.Pipe()
	go il.Handle(server, bumped+":443")
	if got := over(t, client, roots, bumped); got != "bumped" {
		t.Errorf("got %q, want bumped", got)
	}
	srv.Close()
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve returned %v after Close", err)
	}
}

func TestHandleReturnsWhenBumpedConnectionCloses(t *testing.T) {
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	il := New(ca, func(n string) bool { return n == bumped })
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "bumped") })}
	go srv.Serve(il)
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	server, client := net.Pipe()
	handled := make(chan struct{})
	go func() { il.Handle(server, bumped+":443"); close(handled) }()
	over(t, client, roots, bumped)
	select {
	case <-handled:
		t.Fatal("Handle returned while the tunnel was still open")
	default:
	}
	client.Close()
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("Handle did not return after the connection closed")
	}
}

func TestWithoutCANothingIsBumped(t *testing.T) {
	il := New(nil, func(string) bool { return true })
	real, _ := realServer(t)
	c := tls.Client(handle(t, il, real.Listener.Addr().String()), &tls.Config{ServerName: bumped, InsecureSkipVerify: true})
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	if !c.ConnectionState().PeerCertificates[0].Equal(real.Certificate()) {
		t.Error("a configured name was bumped without a CA")
	}
}

// toward is a connection that knows where its client was going, as one read
// from a PROXY protocol header does.
type toward struct {
	net.Conn
	dst string
}

func (c toward) Destination() string { return c.dst }

type redirecting struct {
	net.Listener
	dst string
}

func (l redirecting) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return toward{c, l.dst}, nil
}

func TestRedirectedConnectionPassesThroughToItsDestination(t *testing.T) {
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialed := make(chan string, 1)
	il := newListener(redirecting{raw, "160.79.104.10:443"}, ca, func(n string) bool { return n == bumped },
		func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed <- addr
			return nil, errors.New("not dialing in a test")
		})
	t.Cleanup(func() { il.Close() })
	go get(raw.Addr().String(), nil, "other.example.test", true)
	select {
	case got := <-dialed:
		if got != "160.79.104.10:443" {
			t.Errorf("dialed %q, want the connection's destination 160.79.104.10:443", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never dialed")
	}
}
