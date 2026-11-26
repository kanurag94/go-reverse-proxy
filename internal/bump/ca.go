// Package bump terminates TLS for configured names with certificates minted on
// the fly from a local CA, so the proxy can read traffic meant for those hosts.
package bump

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA signs leaf certificates for the names the proxy bumps.
type CA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	leafKey *ecdsa.PrivateKey // shared by every leaf, so minting is just signing (as mitmproxy does)
	now     func() time.Time

	mu     sync.Mutex
	leaves map[string]*tls.Certificate // by name
}

// LoadOrCreate loads the CA in dir (ca.pem, ca-key.pem), creating it on first use.
func LoadOrCreate(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if _, err := os.Stat(certPath); errors.Is(err, fs.ErrNotExist) {
		if err := create(dir, certPath, keyPath); err != nil {
			return nil, fmt.Errorf("create CA: %w", err)
		}
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || !pair.Leaf.IsCA {
		return nil, fmt.Errorf("%s is not a CA certificate with a signing key", certPath)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &CA{cert: pair.Leaf, key: key, leafKey: leafKey, now: time.Now, leaves: make(map[string]*tls.Certificate)}, nil
}

func (c *CA) Cert() *x509.Certificate { return c.cert }

// create writes a new self-signed CA. The key goes first, so a crash between the
// two writes leaves no ca.pem and the next start creates a fresh pair.
func create(dir, certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := caTemplate()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writePEM(keyPath, "PRIVATE KEY", keyDER, 0o600); err != nil {
		return err
	}
	return writePEM(certPath, "CERTIFICATE", der, 0o644)
}

// caTemplate is a CA valid for ten years that may sign leaves but no other CA.
func caTemplate() *x509.Certificate {
	now := time.Now()
	return &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "go-reverse-proxy local CA"},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
}

func writePEM(path, kind string, der []byte, perm os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), perm)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}
