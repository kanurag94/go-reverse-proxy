package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/config"
)

// writeCert writes a self-signed certificate for name and its key as PEM files in dir.
func writeCert(t *testing.T, dir, name string) (config.Cert, *x509.Certificate) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	c := config.Cert{CertFile: filepath.Join(dir, name+".crt"), KeyFile: filepath.Join(dir, name+".key")}
	writePEM(t, c.CertFile, "CERTIFICATE", der)
	writePEM(t, c.KeyFile, "PRIVATE KEY", keyDER)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, cert
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTLSPicksCertificateBySNI(t *testing.T) {
	dir := t.TempDir()
	a, certA := writeCert(t, dir, "a.local")
	b, certB := writeCert(t, dir, "b.local")
	cfg, err := tlsConfig([]config.Cert{a, b})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()

	for name, cert := range map[string]*x509.Certificate{"a.local": certA, "b.local": certB} {
		roots := x509.NewCertPool()
		roots.AddCert(cert) // trusting only this name's certificate: the handshake verifies the choice
		conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{ServerName: name, RootCAs: roots})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		conn.Close()
	}
}

func TestTLSMissingCertFile(t *testing.T) {
	dir := t.TempDir()
	a, _ := writeCert(t, dir, "a.local")
	missing := config.Cert{CertFile: filepath.Join(dir, "gone.crt"), KeyFile: filepath.Join(dir, "gone.key")}

	_, err := tlsConfig([]config.Cert{a, missing})
	if err == nil || !strings.Contains(err.Error(), "tls 1") || !strings.Contains(err.Error(), "gone.crt") {
		t.Errorf("err = %v, want it to name tls 1 and gone.crt", err)
	}
}
