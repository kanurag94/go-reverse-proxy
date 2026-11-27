package upstream_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

// fakeDNS answers lookups from a table the test can change.
type fakeDNS struct {
	mu      sync.Mutex
	answers map[string][]string
	fail    bool
	lookups atomic.Int32
}

func (f *fakeDNS) set(host string, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[host] = ips
}

func (f *fakeDNS) failing(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.lookups.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("dns is down")
	}
	var out []net.IPAddr
	for _, ip := range f.answers[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}

// network maps fake addresses to local servers and counts dials to each.
type network struct {
	routes map[string]string // "10.0.0.1:80" -> a real local address
	dials  sync.Map          // fake address -> *atomic.Int32
}

func (n *network) dial(ctx context.Context, proto, addr string) (net.Conn, error) {
	c, _ := n.dials.LoadOrStore(addr, new(atomic.Int32))
	c.(*atomic.Int32).Add(1)
	real, ok := n.routes[addr]
	if !ok {
		return nil, errors.New("connection refused: " + addr)
	}
	return (&net.Dialer{}).DialContext(ctx, proto, real)
}

func (n *network) dialed(addr string) int32 {
	c, ok := n.dials.Load(addr)
	if !ok {
		return 0
	}
	return c.(*atomic.Int32).Load()
}

// named answers with its name and the Host header it was sent.
func named(t *testing.T, name string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, name+" "+r.Host)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// resolving builds a pool for http://svc.test that resolves through dns and
// dials through n, refreshing every 10ms until the test ends.
func resolving(t *testing.T, dns *fakeDNS, n *network) *upstream.Pool {
	t.Helper()
	u, _ := url.Parse("http://svc.test")
	base := &http.Transport{DialContext: n.dial}
	p := upstream.NewPool([]*upstream.Endpoint{upstream.NewEndpoint(u, base)}, "", upstream.Health{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.FollowDNS(ctx, dns, 10*time.Millisecond)
	return p
}

func fetch(t *testing.T, p *upstream.Pool) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	res, err := p.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func eventually(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

func TestEachAddressIsAMember(t *testing.T) {
	dns := &fakeDNS{answers: map[string][]string{"svc.test": {"10.0.0.1", "10.0.0.2"}}}
	n := &network{routes: map[string]string{"10.0.0.1:80": named(t, "a"), "10.0.0.2:80": named(t, "b")}}
	p := resolving(t, dns, n)
	got := map[string]bool{fetch(t, p): true, fetch(t, p): true}
	if !got["a svc.test"] || !got["b svc.test"] {
		t.Errorf("served by %v, want both addresses, each sent Host svc.test", got)
	}
}

func TestUnreachableAddressIsEjected(t *testing.T) {
	dns := &fakeDNS{answers: map[string][]string{"svc.test": {"10.0.0.1", "10.0.0.9"}}}
	n := &network{routes: map[string]string{"10.0.0.1:80": named(t, "a")}}
	p := resolving(t, dns, n)
	for range 4 {
		if got := fetch(t, p); got != "a svc.test" {
			t.Fatalf("got %q, want a", got)
		}
	}
	if d := n.dialed("10.0.0.9:80"); d != 1 {
		t.Errorf("dialed the dead address %d times, want once before it was ejected", d)
	}
}

func TestRefreshFollowsDNS(t *testing.T) {
	dns := &fakeDNS{answers: map[string][]string{"svc.test": {"10.0.0.1"}}}
	n := &network{routes: map[string]string{"10.0.0.1:80": named(t, "a"), "10.0.0.2:80": named(t, "b")}}
	p := resolving(t, dns, n)
	dns.set("svc.test", "10.0.0.2")
	eventually(t, func() bool { return fetch(t, p) == "b svc.test" }, "never moved to the new address")
	for range 3 {
		if got := fetch(t, p); got != "b svc.test" {
			t.Errorf("got %q after the old address was removed", got)
		}
	}
}

func TestRefreshKeepsHealth(t *testing.T) {
	dns := &fakeDNS{answers: map[string][]string{"svc.test": {"10.0.0.1", "10.0.0.9"}}}
	n := &network{routes: map[string]string{"10.0.0.1:80": named(t, "a")}}
	p := resolving(t, dns, n)
	fetch(t, p)
	fetch(t, p) // the dead address is now ejected
	before := dns.lookups.Load()
	eventually(t, func() bool { return dns.lookups.Load() > before+2 }, "never refreshed")
	fetch(t, p)
	fetch(t, p)
	if d := n.dialed("10.0.0.9:80"); d != 1 {
		t.Errorf("dialed the dead address %d times; a refresh must not forget it was ejected", d)
	}
}

func TestFailedRefreshKeepsLastAddresses(t *testing.T) {
	dns := &fakeDNS{answers: map[string][]string{"svc.test": {"10.0.0.1"}}}
	n := &network{routes: map[string]string{"10.0.0.1:80": named(t, "a")}}
	p := resolving(t, dns, n)
	dns.failing(true)
	before := dns.lookups.Load()
	eventually(t, func() bool { return dns.lookups.Load() > before+2 }, "never refreshed")
	if got := fetch(t, p); got != "a svc.test" {
		t.Errorf("got %q, want the last good address", got)
	}
}

func TestResolveStopsWithItsContext(t *testing.T) {
	dns := &fakeDNS{answers: map[string][]string{"svc.test": {"10.0.0.1"}}}
	u, _ := url.Parse("http://svc.test")
	p := upstream.NewPool([]*upstream.Endpoint{upstream.NewEndpoint(u, &http.Transport{})}, "", upstream.Health{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	p.FollowDNS(ctx, dns, 5*time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	stopped := dns.lookups.Load()
	time.Sleep(50 * time.Millisecond)
	if dns.lookups.Load() != stopped {
		t.Error("still resolving after the context ended")
	}
}
