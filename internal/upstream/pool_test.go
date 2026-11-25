package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var errRefused = errors.New("connect: connection refused")

// base stands in for the Transport: it records which host each request reached,
// and fails or answers per host as the test says.
type base struct {
	mu     sync.Mutex
	got    []string
	fail   map[string]bool
	status map[string]int
}

func (b *base) RoundTrip(r *http.Request) (*http.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = append(b.got, r.URL.Host)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if b.fail[r.URL.Host] {
		return nil, errRefused
	}
	code := http.StatusOK
	if c := b.status[r.URL.Host]; c != 0 {
		code = c
	}
	return &http.Response{StatusCode: code, Body: http.NoBody, Request: r}, nil
}

func (b *base) setFail(host string, fail bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail[host] = fail
}

func (b *base) hosts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.got)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestPool(t *testing.T, policy string, health Health, hosts ...string) (*Pool, *base, *clock) {
	t.Helper()
	b := &base{fail: map[string]bool{}, status: map[string]int{}}
	var eps []*Endpoint
	for _, h := range hosts {
		u, err := url.Parse("http://" + h)
		if err != nil {
			t.Fatal(err)
		}
		eps = append(eps, NewEndpoint(u, b))
	}
	c := &clock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	p := NewPool(eps, policy, health)
	p.now = c.now
	return p, b, c
}

func send(p *Pool, ctx context.Context) error {
	res, err := p.RoundTrip(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	if err == nil {
		res.Body.Close()
	}
	return err
}

func sendN(p *Pool, n int) {
	for range n {
		send(p, context.Background())
	}
}

func assertHosts(t *testing.T, b *base, want ...string) {
	t.Helper()
	if got := b.hosts(); !slices.Equal(got, want) {
		t.Errorf("requests went to %v, want %v", got, want)
	}
}

func TestRoundRobin(t *testing.T) {
	p, b, _ := newTestPool(t, "round_robin", Health{}, "a", "b", "c")
	sendN(p, 6)
	assertHosts(t, b, "a", "b", "c", "a", "b", "c")
}

func TestDefaultPolicyIsRoundRobin(t *testing.T) {
	p, b, _ := newTestPool(t, "", Health{}, "a", "b")
	sendN(p, 4)
	assertHosts(t, b, "a", "b", "a", "b")
}

func TestRoundRobinIsEvenUnderConcurrency(t *testing.T) {
	p, b, _ := newTestPool(t, "round_robin", Health{}, "a", "b", "c")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { sendN(p, 300) })
	}
	wg.Wait()
	counts := map[string]int{}
	for _, h := range b.hosts() {
		counts[h]++
	}
	if counts["a"] != 800 || counts["b"] != 800 || counts["c"] != 800 {
		t.Errorf("counts = %v, want 800 each", counts)
	}
}

func TestFailoverUsesPrimary(t *testing.T) {
	p, b, _ := newTestPool(t, "failover", Health{}, "a", "b")
	sendN(p, 3)
	assertHosts(t, b, "a", "a", "a")
}

func TestFailoverEjectsAndRecovers(t *testing.T) {
	p, b, clk := newTestPool(t, "failover", Health{}, "a", "b") // defaults: 1 failure, 10s
	b.setFail("a", true)
	if err := send(p, context.Background()); err == nil {
		t.Fatal("the failing request must still fail; retrying it is step 7")
	}
	sendN(p, 2)

	b.setFail("a", false)
	clk.add(9 * time.Second)
	sendN(p, 1)
	clk.add(time.Second)
	sendN(p, 1)
	assertHosts(t, b, "a", "b", "b", "b", "a")
}

func TestMaxFailuresCountsConsecutive(t *testing.T) {
	p, b, _ := newTestPool(t, "failover", Health{MaxFailures: 3, EjectDuration: time.Minute}, "a", "b")
	b.setFail("a", true)
	sendN(p, 2)
	b.setFail("a", false)
	sendN(p, 1) // a success resets the count
	b.setFail("a", true)
	sendN(p, 2)
	assertHosts(t, b, "a", "a", "a", "a", "a")

	sendN(p, 2) // third consecutive failure ejects a
	assertHosts(t, b, "a", "a", "a", "a", "a", "a", "b")
}

func TestRoundRobinSkipsEjected(t *testing.T) {
	p, b, _ := newTestPool(t, "round_robin", Health{}, "a", "b", "c")
	b.setFail("b", true)
	sendN(p, 6)
	assertHosts(t, b, "a", "b", "a", "c", "a", "c") // the healthy ones share b's traffic evenly
}

func TestNotEjected(t *testing.T) {
	t.Run("client cancelled", func(t *testing.T) {
		p, b, _ := newTestPool(t, "failover", Health{}, "a", "b")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		send(p, ctx)
		sendN(p, 1)
		assertHosts(t, b, "a", "a")
	})
	t.Run("server error response", func(t *testing.T) {
		p, b, _ := newTestPool(t, "failover", Health{}, "a", "b")
		b.status["a"] = http.StatusInternalServerError
		sendN(p, 2)
		assertHosts(t, b, "a", "a")
	})
}

func TestAllEjectedStillTries(t *testing.T) {
	p, b, _ := newTestPool(t, "failover", Health{}, "a", "b")
	b.setFail("a", true)
	b.setFail("b", true)
	sendN(p, 3)
	if got := b.hosts(); len(got) != 3 || !strings.Contains(strings.Join(got, ""), "a") {
		t.Errorf("requests went to %v: with every endpoint ejected, the pool must still try one", got)
	}
}
