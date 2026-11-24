package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/config"
)

const validJSON = `{
  "listeners": [{
    "name": "public",
    "address": ":8443",
    "tls": [{"cert_file": "api.crt", "key_file": "api.key"}],
    "hosts": [{
      "name": "api",
      "domains": ["api.example.com"],
      "routes": [
        {"prefix": "/v1", "upstream": "api-v1"},
        {"prefix": "/", "upstream": "api"}
      ]
    }]
  }],
  "upstreams": [
    {"name": "api", "endpoints": ["http://127.0.0.1:9001"]},
    {"name": "api-v1", "policy": "failover", "endpoints": ["http://127.0.0.1:9002", "http://127.0.0.1:9003"]}
  ]
}`

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := config.Load(writeFile(t, validJSON))
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.Listeners[0]
	if l.Name != "public" || l.Address != ":8443" || l.TLS[0].CertFile != "api.crt" {
		t.Errorf("listener = %+v", l)
	}
	if r := l.Hosts[0].Routes[1]; r.Prefix != "/" || r.Upstream != "api" {
		t.Errorf("route = %+v", r)
	}
	if u := cfg.Upstreams[1]; u.Name != "api-v1" || u.Policy != "failover" || len(u.Endpoints) != 2 {
		t.Errorf("upstream = %+v", u)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"unknown field", `{"listeners": [], "upstrem": {}}`, `unknown field "upstrem"`},
		{"trailing data", `{"listeners": []} {}`, "data after the top-level object"},
		{"truncated", `{"listeners": [`, "unexpected EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeFile(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}
