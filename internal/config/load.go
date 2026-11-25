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

type Listener struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	TLS     []Cert `json:"tls"`
	Hosts   []Host `json:"hosts"`
}

type Cert struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type Host struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	Routes  []Route  `json:"routes"`
}

type Route struct {
	Prefix   string `json:"prefix"`
	Upstream string `json:"upstream"`
}

type Upstream struct {
	Name      string         `json:"name"`
	Policy    string         `json:"policy"`
	Endpoints []string       `json:"endpoints"`
	Health    Health         `json:"health"`
	Timeouts  Timeouts       `json:"timeouts"`
	Retries   Retries        `json:"retries"`
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
