package bump_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/bump"
)

func newCA(t *testing.T) *bump.CA {
	t.Helper()
	ca, err := bump.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func only(names ...string) func(string) bool {
	return func(n string) bool { return slices.Contains(names, n) }
}

// handshake connects to a bumping server as name, trusting only ca, offering protos.
func handshake(t *testing.T, ca *bump.CA, allowed func(string) bool, name string, protos ...string) (*tls.Conn, error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ca.TLSConfig(allowed))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.(*tls.Conn).Handshake()
			io.Copy(io.Discard, c) // hold the conn until the client closes it
			c.Close()
		}
	}()
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: name, RootCAs: roots, NextProtos: protos})
	if err == nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, err
}

func TestMintedLeafIsTrusted(t *testing.T) {
	conn, err := handshake(t, newCA(t), only("api.example.test"), "api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if leaf := conn.ConnectionState().PeerCertificates[0]; !slices.Equal(leaf.DNSNames, []string{"api.example.test"}) {
		t.Errorf("leaf names = %q, want api.example.test", leaf.DNSNames)
	}
}

func TestUnconfiguredNameIsRefused(t *testing.T) {
	if _, err := handshake(t, newCA(t), only("api.example.test"), "other.example.test"); err == nil {
		t.Error("handshake succeeded for a name nobody configured")
	}
}

func TestALPN(t *testing.T) {
	ca := newCA(t)
	for _, tc := range []struct {
		offer []string
		want  string // "" means the handshake fails
	}{
		{[]string{"h2", "http/1.1"}, "h2"},
		{[]string{"http/1.1"}, "http/1.1"},
		{[]string{"acme-tls/1"}, ""},
	} {
		conn, err := handshake(t, ca, only("api.example.test"), "api.example.test", tc.offer...)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("offer %q: handshake succeeded, want it refused", tc.offer)
		case tc.want != "" && err != nil:
			t.Errorf("offer %q: %v", tc.offer, err)
		case tc.want != "" && conn.ConnectionState().NegotiatedProtocol != tc.want:
			t.Errorf("offer %q: got %q, want %q", tc.offer, conn.ConnectionState().NegotiatedProtocol, tc.want)
		}
	}
}

func TestLeafIsCached(t *testing.T) {
	ca := newCA(t)
	serial := func() *big.Int {
		conn, err := handshake(t, ca, only("api.example.test"), "api.example.test")
		if err != nil {
			t.Fatal(err)
		}
		return conn.ConnectionState().PeerCertificates[0].SerialNumber
	}
	if a, b := serial(), serial(); a.Cmp(b) != 0 {
		t.Errorf("serials %v and %v, want the cached leaf reused", a, b)
	}
}

func TestCAIsCreatedThenLoaded(t *testing.T) {
	dir := t.TempDir()
	first, err := bump.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := bump.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Cert().Equal(second.Cert()) || !first.Cert().IsCA {
		t.Error("second start did not load the CA the first created")
	}
	fi, err := os.Stat(filepath.Join(dir, "ca-key.pem"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("ca-key.pem: %v; want mode 0600", err)
	}
}
