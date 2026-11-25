package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/config"
)

const validJSON = `{
  "cache": {"max_size": 1048576, "max_response_size": 65536},
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
    {"name": "api", "endpoints": ["http://127.0.0.1:9001"],
     "cache": {"enabled": true, "private_headers": ["X-Api-Key"]}},
    {"name": "api-v1", "policy": "failover", "endpoints": ["http://127.0.0.1:9002", "http://127.0.0.1:9003"],
     "health": {"max_failures": 3, "eject_duration": "30s"},
     "timeouts": {"connect": "2s", "response_header": "10m"},
     "retries": {"attempts": 3}}
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
	if c := cfg.Cache; c == nil || c.MaxSize != 1<<20 || c.MaxResponseSize != 1<<16 {
		t.Errorf("cache = %+v", c)
	}
	if c := cfg.Upstreams[0].Cache; c == nil || !c.Enabled || len(c.PrivateHeaders) != 1 || c.PrivateHeaders[0] != "X-Api-Key" {
		t.Errorf("api cache = %+v", c)
	}
	if h := cfg.Upstreams[1].Health; h.MaxFailures != 3 || time.Duration(h.EjectDuration) != 30*time.Second {
		t.Errorf("health = %+v", h)
	}
	if to := cfg.Upstreams[1].Timeouts; time.Duration(to.Connect) != 2*time.Second || time.Duration(to.ResponseHeader) != 10*time.Minute {
		t.Errorf("timeouts = %+v", to)
	}
	if a := cfg.Upstreams[1].Retries.Attempts; a != 3 {
		t.Errorf("retries.attempts = %d, want 3", a)
	}
	if cfg.Upstreams[1].CacheEnabled() {
		t.Error("api-v1 has no cache block, so it must not be cached")
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"unknown field", `{"listeners": [], "upstrem": {}}`, `unknown field "upstrem"`},
		{"trailing data", `{"listeners": []} {}`, "data after the top-level object"},
		{"truncated", `{"listeners": [`, "unexpected EOF"},
		{"duration as a number", `{"upstreams": [{"timeouts": {"connect": 5}}]}`, `duration must be a string like "10s"`},
		{"duration not parsable", `{"upstreams": [{"timeouts": {"connect": "soon"}}]}`, `invalid duration "soon"`},
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
