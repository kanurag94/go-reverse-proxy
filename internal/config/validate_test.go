package config_test

import (
	"strings"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/config"
)

func validConfig() *config.Config {
	return &config.Config{
		Listeners: []config.Listener{{
			Name:    "public",
			Address: ":8443",
			TLS:     []config.Cert{{CertFile: "api.crt", KeyFile: "api.key"}},
			Hosts: []config.Host{{
				Name:    "api",
				Domains: []string{"api.example.com"},
				Routes: []config.Route{
					{Prefix: "/v1", Upstream: "api-v1"},
					{Prefix: "/", Upstream: "api"},
				},
			}},
		}},
		Upstreams: []config.Upstream{
			{Name: "api", Endpoints: []string{"http://127.0.0.1:9001"}},
			{Name: "api-v1", Policy: "failover", Endpoints: []string{
				"http://127.0.0.1:9002",
				"https://backup.example.com/base?k=v",
			}},
		},
	}
}

func listener(c *config.Config) *config.Listener { return &c.Listeners[0] }
func host(c *config.Config) *config.Host         { return &c.Listeners[0].Hosts[0] }
func upstream(c *config.Config) *config.Upstream { return &c.Upstreams[0] }

func TestValidateOK(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePrefixIsSegmentAware(t *testing.T) {
	c := validConfig()
	host(c).Routes = []config.Route{
		{Prefix: "/v1", Upstream: "api"},
		{Prefix: "/v10", Upstream: "api"},
		{Prefix: "/", Upstream: "api"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("/v1 must not cover /v10: %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *config.Config)
		want   string
	}{
		{"no listeners", func(c *config.Config) { c.Listeners = nil }, "no listeners"},
		{"listener without name", func(c *config.Config) { listener(c).Name = "" }, "listener 0: empty name"},
		{"duplicate listener name", func(c *config.Config) {
			second := *listener(c)
			second.Address = ":9000"
			c.Listeners = append(c.Listeners, second)
		}, `listener "public": duplicate name`},
		{"address without port", func(c *config.Config) { listener(c).Address = "8443" }, `listener "public": address "8443"`},
		{"duplicate address", func(c *config.Config) {
			second := *listener(c)
			second.Name = "other"
			c.Listeners = append(c.Listeners, second)
		}, `listener "other": address ":8443" already used by listener "public"`},
		{"cert without key", func(c *config.Config) { listener(c).TLS[0].KeyFile = "" }, `listener "public": tls 0: cert_file and key_file are required`},
		{"no hosts", func(c *config.Config) { listener(c).Hosts = nil }, `listener "public": no hosts`},

		{"host without name", func(c *config.Config) { host(c).Name = "" }, `listener "public": host 0: empty name`},
		{"duplicate host name", func(c *config.Config) {
			second := *host(c)
			second.Domains = []string{"other.example.com"}
			listener(c).Hosts = append(listener(c).Hosts, second)
		}, `host "api": duplicate name`},
		{"no domains", func(c *config.Config) { host(c).Domains = nil }, `host "api": no domains`},
		{"empty domain", func(c *config.Config) { host(c).Domains = []string{""} }, `host "api": empty domain`},
		{"uppercase domain", func(c *config.Config) { host(c).Domains = []string{"API.example.com"} }, `domain "API.example.com" must be lowercase`},
		{"domain with port", func(c *config.Config) { host(c).Domains = []string{"api.example.com:443"} }, `domain "api.example.com:443" must not have a port`},
		{"domain in two hosts", func(c *config.Config) {
			second := *host(c)
			second.Name = "api2"
			listener(c).Hosts = append(listener(c).Hosts, second)
		}, `host "api2": domain "api.example.com" already used by host "api"`},
		{"no routes", func(c *config.Config) { host(c).Routes = nil }, `host "api": no routes`},

		{"prefix without leading slash", func(c *config.Config) { host(c).Routes[0].Prefix = "v1" }, `route "v1": prefix must start with "/"`},
		{"prefix with trailing slash", func(c *config.Config) { host(c).Routes[0].Prefix = "/v1/" }, `route "/v1/": prefix must not end with "/"`},
		{"route after catch-all", func(c *config.Config) {
			host(c).Routes = []config.Route{{Prefix: "/", Upstream: "api"}, {Prefix: "/v1", Upstream: "api"}}
		}, `route "/v1": unreachable, covered by "/"`},
		{"route after parent", func(c *config.Config) {
			host(c).Routes = []config.Route{{Prefix: "/v1", Upstream: "api"}, {Prefix: "/v1/chat", Upstream: "api"}}
		}, `route "/v1/chat": unreachable, covered by "/v1"`},
		{"duplicate prefix", func(c *config.Config) {
			host(c).Routes = []config.Route{{Prefix: "/v1", Upstream: "api"}, {Prefix: "/v1", Upstream: "api"}}
		}, `route "/v1": unreachable, covered by "/v1"`},
		{"unknown upstream", func(c *config.Config) { host(c).Routes[0].Upstream = "nope" }, `route "/v1": unknown upstream "nope"`},

		{"upstream without name", func(c *config.Config) { upstream(c).Name = "" }, "upstream 0: empty name"},
		{"duplicate upstream name", func(c *config.Config) { c.Upstreams[1].Name = "api" }, `upstream "api": duplicate name`},
		{"unknown policy", func(c *config.Config) { upstream(c).Policy = "random" }, `upstream "api": unknown policy "random"`},
		{"no endpoints", func(c *config.Config) { upstream(c).Endpoints = nil }, `upstream "api": no endpoints`},
		{"endpoint not a URL", func(c *config.Config) { upstream(c).Endpoints = []string{"http://[::1"} }, `upstream "api": endpoint 0: invalid URL`},
		{"endpoint bad scheme", func(c *config.Config) { upstream(c).Endpoints = []string{"ftp://h"} }, "endpoint 0: scheme must be http or https"},
		{"endpoint without host", func(c *config.Config) { upstream(c).Endpoints = []string{"http:///path"} }, "endpoint 0: missing host"},
		{"endpoint with user info", func(c *config.Config) { upstream(c).Endpoints = []string{"http://u:secret@h"} }, "endpoint 0: must not contain user info"},
		{"endpoint with fragment", func(c *config.Config) { upstream(c).Endpoints = []string{"http://h/#x"} }, "endpoint 0: must not have a fragment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks a credential: %v", err)
			}
		})
	}
}
