package intercept

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/bump"
)

// client starts a TLS handshake on its end of a pipe and returns the other end.
// done gets the handshake's result once a server answers (or the pipe closes).
func client(t *testing.T, cfg *tls.Config, wrap func(net.Conn) net.Conn) (server net.Conn, done <-chan error) {
	t.Helper()
	s, c := net.Pipe()
	t.Cleanup(func() { s.Close(); c.Close() })
	ch := make(chan error, 1)
	go func() { ch <- tls.Client(wrap(c), cfg).Handshake() }()
	return s, ch
}

func same(c net.Conn) net.Conn { return c }

// trickle writes one byte at a time, so the ClientHello arrives in pieces.
type trickle struct{ net.Conn }

func (c trickle) Write(b []byte) (int, error) {
	for i := range b {
		if _, err := c.Conn.Write(b[i : i+1]); err != nil {
			return i, err
		}
	}
	return len(b), nil
}

func TestReadHelloGetsSNIAndALPN(t *testing.T) {
	conn, _ := client(t, &tls.Config{ServerName: "API.example.test.", NextProtos: []string{"h2", "http/1.1"}}, same)
	h, _, err := readHello(conn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if h.sni != "api.example.test" || !slices.Equal(h.protos, []string{"h2", "http/1.1"}) {
		t.Errorf("hello = %+v, want sni api.example.test (lowercase, no trailing dot) and h2, http/1.1", h)
	}
}

// The replayed conn must look untouched: a real handshake on it completes, so the
// probe consumed nothing and sent nothing. Also when the hello comes in pieces.
func TestReplayCompletesTheRealHandshake(t *testing.T) {
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	for label, wrap := range map[string]func(net.Conn) net.Conn{"whole": same, "trickled": func(c net.Conn) net.Conn { return trickle{c} }} {
		conn, done := client(t, &tls.Config{ServerName: "api.example.test", RootCAs: roots}, wrap)
		_, replay, err := readHello(conn, 5*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		go tls.Server(replay, ca.TLSConfig(func(string) bool { return true })).Handshake()
		if err := <-done; err != nil {
			t.Errorf("%s: client handshake after replay: %v", label, err)
		}
	}
}

func TestReadHelloPassesNonTLS(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	go io.WriteString(c, "GET / HTTP/1.1\r\n\r\n")
	h, replay, err := readHello(s, 5*time.Second)
	if err != nil || h.sni != "" {
		t.Fatalf("hello %+v, err %v; want an empty hello and no error", h, err)
	}
	buf := make([]byte, 18)
	if _, err := io.ReadFull(replay, buf); err != nil || string(buf) != "GET / HTTP/1.1\r\n\r\n" {
		t.Errorf("replayed %q, %v; want the bytes as sent", buf, err)
	}
}

func TestReadHelloGivesUpOnSilence(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	if _, _, err := readHello(s, 50*time.Millisecond); err == nil {
		t.Error("readHello returned without a hello or a timeout")
	}
}

// The deadline guards only the probe: a handshake that starts after it has
// passed must still complete.
func TestDeadlineIsClearedAfterTheHello(t *testing.T) {
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	conn, _ := client(t, &tls.Config{ServerName: "api.example.test", RootCAs: roots}, same)
	_, replay, err := readHello(conn, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // past the probe's deadline
	server := make(chan error, 1)
	go func() { server <- tls.Server(replay, ca.TLSConfig(func(string) bool { return true })).Handshake() }()
	select {
	case err := <-server:
		if err != nil {
			t.Errorf("handshake after the probe's deadline: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not finish")
	}
}
