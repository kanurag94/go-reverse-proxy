package bump

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"strings"
	"time"
)

const (
	leafLifetime = 24 * time.Hour
	renewBefore  = time.Hour // mint a new leaf once the cached one is this close to expiry
	backdate     = time.Hour // for clients whose clock is a little behind
)

// TLSConfig terminates TLS for the names allowed reports, minting a leaf for each;
// any other name fails the handshake. ALPN offers h2 then http/1.1; a client
// offering neither is refused, as crypto/tls does when no protocol is shared.
func (c *CA) TLSConfig(allowed func(name string) bool) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
			if name == "" || !allowed(name) {
				return nil, fmt.Errorf("bump: %q is not a configured domain", hello.ServerName)
			}
			return c.leaf(name)
		},
	}
}

// leaf returns name's cached certificate while it is fresh, and mints a new one
// otherwise. Minting holds the lock: it takes about a millisecond, and names are few.
func (c *CA) leaf(name string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.leaves[name]; l != nil && c.now().Before(l.Leaf.NotAfter.Add(-renewBefore)) {
		return l, nil
	}
	l, err := c.mint(name)
	if err != nil {
		return nil, err
	}
	c.leaves[name] = l
	return l, nil
}

// mint signs a leaf for name. The CA is the root clients trust, so the leaf alone
// is the whole chain.
func (c *CA) mint(name string) (*tls.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, leafTemplate(name, c.now()), c.cert, &c.leafKey.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: c.leafKey, Leaf: leaf}, nil
}

// leafTemplate is a server certificate for name alone, valid for leafLifetime.
func leafTemplate(name string, now time.Time) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}
