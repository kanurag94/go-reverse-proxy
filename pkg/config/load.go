// Package config is the proxy's configuration: listeners route hosts and paths
// to upstreams. It is read from JSON strictly, or built in Go by an embedder.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Config is the whole configuration; Validate checks it.
type Config struct {
	Cache     *Cache     `json:"cache"`
	Listeners []Listener `json:"listeners"`
	Upstreams []Upstream `json:"upstreams"`
}

// Cache sizes the one response store that all cached upstreams share, in bytes.
// Zero or missing means the default.
type Cache struct {
	MaxSize         int64 `json:"max_size"`          // all stored responses together
	MaxResponseSize int64 `json:"max_response_size"` // one stored response
}

// Listener accepts connections on Address, over TLS when it has certificates or
// bumps, and routes its Hosts.
type Listener struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	TLS     []Cert `json:"tls"`
	Bump    *Bump  `json:"bump"`
	Connect bool   `json:"connect"` // accept CONNECT requests from proxy clients
	Hooks   []Hook `json:"hooks"`
	Hosts   []Host `json:"hosts"`
}

// Hook names a registered plugin to run on a listener, in list order. Config is
// passed to the plugin unparsed.
type Hook struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
}

// Bump terminates TLS with certificates minted on the fly for the listener's
// domains, signed by the CA in CADir (created there on first start).
type Bump struct {
	CADir string `json:"ca_dir"`
}

// Cert is a certificate and its key, as PEM files.
type Cert struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

// Host routes requests for its Domains; the first route whose prefix covers the path wins.
type Host struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	Routes  []Route  `json:"routes"`
}

// Route sends paths under Prefix to the upstream named Upstream.
type Route struct {
	Prefix   string `json:"prefix"`
	Upstream string `json:"upstream"`
}

// Upstream is a named set of endpoints and how to reach them.
type Upstream struct {
	Name      string         `json:"name"`
	Policy    string         `json:"policy"`
	Endpoints []string       `json:"endpoints"`
	Health    Health         `json:"health"`
	Timeouts  Timeouts       `json:"timeouts"`
	Retries   Retries        `json:"retries"`
	TLS       *UpstreamTLS   `json:"tls"`
	Cache     *UpstreamCache `json:"cache"`
}

// Health tunes passive health checking. Zero values mean the defaults.
type Health struct {
	MaxFailures   int      `json:"max_failures"`   // consecutive failures to reach an endpoint before it is skipped
	EjectDuration Duration `json:"eject_duration"` // how long it is skipped
}

// Timeouts bound the upstream's connections. Zero values mean the defaults.
type Timeouts struct {
	Connect        Duration `json:"connect"`         // establishing the TCP connection
	ResponseHeader Duration `json:"response_header"` // from the request sent until the response headers arrive
}

// Retries caps how many endpoints one request may try when they cannot be reached.
// Zero means the default of 2; 1 turns retries off.
type Retries struct {
	Attempts int `json:"attempts"`
}

// UpstreamTLS verifies the endpoints against the PEM roots in CAFile instead of the system's.
type UpstreamTLS struct {
	CAFile string `json:"ca_file"`
}

// Duration is a time.Duration written in JSON as a string such as "10s" or "1m30s".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`duration must be a string like "10s"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// UpstreamCache turns caching on for an upstream.
type UpstreamCache struct {
	Enabled bool `json:"enabled"`
	// PrivateHeaders are request headers that identify a user, like Authorization
	// and Cookie: responses to such requests are stored only if marked shareable.
	PrivateHeaders []string `json:"private_headers"`
}

const (
	defaultCacheSize    = 64 << 20 // 64 MiB
	defaultResponseSize = 1 << 20  // 1 MiB
)

// CacheLimits returns the store's sizes in bytes, with defaults for any not set.
func (c *Config) CacheLimits() (maxSize, maxResponseSize int64) {
	maxSize, maxResponseSize = defaultCacheSize, defaultResponseSize
	if c.Cache != nil {
		if c.Cache.MaxSize > 0 {
			maxSize = c.Cache.MaxSize
		}
		if c.Cache.MaxResponseSize > 0 {
			maxResponseSize = c.Cache.MaxResponseSize
		}
	}
	return maxSize, maxResponseSize
}

// CacheEnabled reports whether this upstream's responses are cached: only when
// its config says so explicitly.
func (u Upstream) CacheEnabled() bool {
	return u.Cache != nil && u.Cache.Enabled
}

// CAFile is the file of roots u's endpoints are verified against; "" means the system's.
func (u Upstream) CAFile() string {
	if u.TLS == nil {
		return ""
	}
	return u.TLS.CAFile
}

// BumpDir is the CA dir of a bumping listener; "" means it does not bump.
func (l Listener) BumpDir() string {
	if l.Bump == nil {
		return ""
	}
	return l.Bump.CADir
}

// Load reads and parses the config file at path; it does not validate it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes a config strictly: unknown fields and data after the object are errors.
func Parse(data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("data after the top-level object")
	}
	return &cfg, nil
}
