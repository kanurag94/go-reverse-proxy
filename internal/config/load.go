package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type Config struct {
	Listeners []Listener `json:"listeners"`
	Upstreams []Upstream `json:"upstreams"`
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
	Name      string   `json:"name"`
	Policy    string   `json:"policy"`
	Endpoints []string `json:"endpoints"`
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
