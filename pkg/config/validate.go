package config

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Validate reports the first problem in c, naming where it is.
func (c *Config) Validate() error {
	if len(c.Listeners) == 0 {
		return errors.New("no listeners")
	}
	if c.Cache != nil {
		if err := checkCache(*c.Cache); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
	}
	upstreams := make(map[string]bool)
	for i, u := range c.Upstreams {
		if err := checkUpstream(u, upstreams); err != nil {
			return fmt.Errorf("upstream %s: %w", label(u.Name, i), err)
		}
		upstreams[u.Name] = true
	}
	names := make(map[string]bool)
	addrs := make(map[string]string)
	for i, l := range c.Listeners {
		if err := checkListener(l, names, addrs, upstreams); err != nil {
			return fmt.Errorf("listener %s: %w", label(l.Name, i), err)
		}
		names[l.Name] = true
		addrs[l.Address] = l.Name
	}
	return nil
}

func label(name string, i int) string {
	if name == "" {
		return strconv.Itoa(i)
	}
	return strconv.Quote(name)
}

func checkUpstream(u Upstream, seen map[string]bool) error {
	switch {
	case u.Name == "":
		return errors.New("empty name")
	case seen[u.Name]:
		return errors.New("duplicate name")
	case u.Policy != "" && u.Policy != "round_robin" && u.Policy != "failover":
		return fmt.Errorf("unknown policy %q", u.Policy)
	case len(u.Endpoints) == 0:
		return errors.New("no endpoints")
	}
	for i, e := range u.Endpoints {
		if err := checkEndpoint(e); err != nil {
			return fmt.Errorf("endpoint %d: %w", i, err)
		}
	}
	if u.H2C {
		for i, e := range u.Endpoints {
			if ep, _ := url.Parse(e); ep.Scheme == "https" {
				return fmt.Errorf("h2c: endpoint %d is https; h2c is for http:// endpoints", i)
			}
		}
	}
	if err := checkHealth(u.Health); err != nil {
		return fmt.Errorf("health: %w", err)
	}
	if err := checkTimeouts(u.Timeouts); err != nil {
		return fmt.Errorf("timeouts: %w", err)
	}
	if u.Retries.Attempts < 0 {
		return errors.New("retries: attempts must not be negative")
	}
	if u.TLS != nil && u.TLS.CAFile == "" {
		return errors.New("tls: ca_file is required")
	}
	if u.Cache != nil {
		for i, h := range u.Cache.PrivateHeaders {
			if err := checkHeaderName(h); err != nil {
				return fmt.Errorf("cache: private header %d: %w", i, err)
			}
		}
	}
	return nil
}

func checkHealth(h Health) error {
	switch {
	case h.MaxFailures < 0:
		return errors.New("max_failures must not be negative")
	case h.EjectDuration < 0:
		return errors.New("eject_duration must not be negative")
	}
	return nil
}

func checkTimeouts(t Timeouts) error {
	switch {
	case t.Connect < 0:
		return errors.New("connect must not be negative")
	case t.ResponseHeader < 0:
		return errors.New("response_header must not be negative")
	}
	return nil
}

func checkCache(c Cache) error {
	switch {
	case c.MaxSize < 0:
		return errors.New("max_size must not be negative")
	case c.MaxResponseSize < 0:
		return errors.New("max_response_size must not be negative")
	}
	return nil
}

// checkHeaderName rejects names that could never match a request header,
// which would silently disable the protection they were configured for.
func checkHeaderName(h string) error {
	switch {
	case h == "":
		return errors.New("empty")
	case strings.ContainsAny(h, " \t:"):
		return fmt.Errorf("invalid name %q", h)
	}
	return nil
}

func checkEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error repeats the whole URL, which may contain credentials
		}
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("scheme must be http or https")
	case u.Host == "":
		return errors.New("missing host")
	case u.User != nil:
		return errors.New("must not contain user info")
	case u.Fragment != "":
		return errors.New("must not have a fragment")
	}
	return nil
}

func checkListener(l Listener, names map[string]bool, addrs map[string]string, upstreams map[string]bool) error {
	switch {
	case l.Name == "":
		return errors.New("empty name")
	case names[l.Name]:
		return errors.New("duplicate name")
	}
	if _, _, err := net.SplitHostPort(l.Address); err != nil {
		return fmt.Errorf("address %q: %w", l.Address, err)
	}
	if other, ok := addrs[l.Address]; ok {
		return fmt.Errorf("address %q already used by listener %q", l.Address, other)
	}
	for i, cert := range l.TLS {
		if cert.CertFile == "" || cert.KeyFile == "" {
			return fmt.Errorf("tls %d: cert_file and key_file are required", i)
		}
	}
	if l.Bump != nil {
		switch {
		case len(l.TLS) > 0:
			return errors.New("bump and tls are exclusive")
		case l.Bump.CADir == "":
			return errors.New("bump: ca_dir is required")
		}
	}
	if l.Connect {
		switch {
		case len(l.TLS) > 0:
			return errors.New("connect and tls are exclusive")
		case !loopback(l.Address):
			return errors.New("connect: address must be loopback")
		}
	}
	hooks := make(map[string]bool)
	for i, h := range l.Hooks {
		switch {
		case h.Name == "":
			return fmt.Errorf("hook %d: empty name", i)
		case hooks[h.Name]:
			return fmt.Errorf("hook %d: %q listed twice", i, h.Name)
		}
		hooks[h.Name] = true
	}
	if len(l.Hosts) == 0 {
		return errors.New("no hosts")
	}
	hostNames := make(map[string]bool)
	domains := make(map[string]string)
	for i, h := range l.Hosts {
		if err := checkHost(h, hostNames, domains, upstreams); err != nil {
			return fmt.Errorf("host %s: %w", label(h.Name, i), err)
		}
		hostNames[h.Name] = true
	}
	return nil
}

func checkHost(h Host, names map[string]bool, domains map[string]string, upstreams map[string]bool) error {
	switch {
	case h.Name == "":
		return errors.New("empty name")
	case names[h.Name]:
		return errors.New("duplicate name")
	case len(h.Domains) == 0:
		return errors.New("no domains")
	}
	for _, d := range h.Domains {
		switch {
		case d == "":
			return errors.New("empty domain")
		case d != strings.ToLower(d):
			return fmt.Errorf("domain %q must be lowercase", d)
		case strings.Contains(d, ":"):
			return fmt.Errorf("domain %q must not have a port", d)
		}
		if other, ok := domains[d]; ok {
			return fmt.Errorf("domain %q already used by host %q", d, other)
		}
		domains[d] = h.Name
	}
	if len(h.Routes) == 0 {
		return errors.New("no routes")
	}
	for i, r := range h.Routes {
		if err := checkRoute(r, h.Routes[:i], upstreams); err != nil {
			return fmt.Errorf("route %q: %w", r.Prefix, err)
		}
	}
	return nil
}

func checkRoute(r Route, earlier []Route, upstreams map[string]bool) error {
	switch {
	case !strings.HasPrefix(r.Prefix, "/"):
		return errors.New(`prefix must start with "/"`)
	case r.Prefix != "/" && strings.HasSuffix(r.Prefix, "/"):
		return errors.New(`prefix must not end with "/"`)
	case !upstreams[r.Upstream]:
		return fmt.Errorf("unknown upstream %q", r.Upstream)
	}
	for _, m := range r.Methods {
		if m == "" || m != strings.ToUpper(m) {
			return fmt.Errorf("method %q must be uppercase", m)
		}
	}
	for i, h := range r.Headers {
		if h.Name == "" {
			return fmt.Errorf("header %d: empty name", i)
		}
		if err := checkHeaderName(h.Name); err != nil {
			return fmt.Errorf("header %d: %w", i, err)
		}
	}
	for _, e := range earlier {
		if Covers(e.Prefix, r.Prefix) && acceptsAll(e, r) {
			return fmt.Errorf("unreachable, covered by %q", e.Prefix)
		}
	}
	return nil
}

// acceptsAll reports whether e accepts every request r does: e allows all of r's
// methods (or any method), and r requires each header e requires.
func acceptsAll(e, r Route) bool {
	if len(e.Methods) > 0 && (len(r.Methods) == 0 || !subset(r.Methods, e.Methods)) {
		return false
	}
	for _, eh := range e.Headers {
		if !slices.ContainsFunc(r.Headers, func(rh HeaderMatch) bool { return rh.implies(eh) }) {
			return false
		}
	}
	return true
}

// implies reports whether every request meeting m also meets o.
func (m HeaderMatch) implies(o HeaderMatch) bool {
	return http.CanonicalHeaderKey(m.Name) == http.CanonicalHeaderKey(o.Name) && (o.Value == "" || o.Value == m.Value)
}

func subset(a, b []string) bool {
	return !slices.ContainsFunc(a, func(s string) bool { return !slices.Contains(b, s) })
}

// Covers reports whether prefix matches path on a segment boundary:
// "/v1" covers "/v1" and "/v1/chat" but not "/v10".
func Covers(prefix, path string) bool {
	return prefix == "/" || path == prefix || strings.HasPrefix(path, prefix+"/")
}

func loopback(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}
