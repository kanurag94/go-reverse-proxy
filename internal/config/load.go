package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	Cache     *UpstreamCache `json:"cache"`
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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("config %s: data after the top-level object", path)
	}
	return &cfg, nil
}
