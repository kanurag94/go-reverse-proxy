package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	errRefused = errors.New("connect: connection refused")
	errReset   = errors.New("connection reset by peer")
)

// base stands in for the Transport: it records which host each request reached,
// and fails or answers per host as the test says.
type base struct {
	mu       sync.Mutex
	got      []string
	bodies   map[string]string
	fail     map[string]bool // fails before sending anything, like a refused dial
	sendFail map[string]bool // fails after writing the request headers
	probe    map[string]bool // reads one body byte, then fails before the headers
	status   map[string]int
}

func (b *base) RoundTrip(r *http.Request) (*http.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	host := r.URL.Host
	b.got = append(b.got, host)
	defer r.Body.Close() // like Transport: always close the body, even on errors
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	switch {
	case b.fail[host]:
		return nil, errRefused
	case b.probe[host]:
		io.ReadFull(r.Body, make([]byte, 1))
		return nil, errReset
	case b.sendFail[host]:
		if tr := httptrace.ContextClientTrace(r.Context()); tr != nil && tr.WroteHeaders != nil {
			tr.WroteHeaders()
		}
		return nil, errReset
	}
	data, _ := io.ReadAll(r.Body)
	b.bodies[host] = string(data)
	code := http.StatusOK
	if c := b.status[host]; c != 0 {
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
	b := &base{
		bodies: map[string]string{}, fail: map[string]bool{}, sendFail: map[string]bool{},
		probe: map[string]bool{}, status: map[string]int{},
	}
	var eps []*Endpoint
	for _, h := range hosts {
		u, err := url.Parse("http://" + h)
		if err != nil {
			t.Fatal(err)
		}
		eps = append(eps, NewEndpoint(u, b))
	}
	c := &clock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	p := NewPool(eps, policy, health, 1) // one attempt: these tests look at selection and health alone
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

// clientBody is a request body from a client, counting how often it is closed.
type clientBody struct {
	*strings.Reader
	closes atomic.Int32
}

func (c *clientBody) Close() error {
	c.closes.Add(1)
	return nil
}

func retryPool(t *testing.T, attempts int, health Health, hosts ...string) (*Pool, *base) {
	t.Helper()
	p, b, _ := newTestPool(t, "failover", health, hosts...)
	p.attempts = attempts
	return p, b
}

func roundTrip(p *Pool, ctx context.Context, method string, body *clientBody) (*http.Response, error) {
	r := httptest.NewRequest(method, "/", nil).WithContext(ctx)
	if body != nil {
		r.Body, r.ContentLength = body, int64(body.Len())
	}
	res, err := p.RoundTrip(r)
	if err == nil {
		res.Body.Close()
	}
	return res, err
}

func TestRetriesWhenNothingWasSent(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			p, b := retryPool(t, 2, Health{}, "a", "b")
			b.fail["a"] = true
			body := &clientBody{Reader: strings.NewReader("payload")}
			if _, err := roundTrip(p, context.Background(), method, body); err != nil {
				t.Fatalf("err = %v, want the retry on b to succeed", err)
			}
			assertHosts(t, b, "a", "b")
			if b.bodies["b"] != "payload" {
				t.Errorf("b received body %q, want the whole body", b.bodies["b"])
			}
			if n := body.closes.Load(); n != 0 {
				t.Errorf("client body closed %d times; a failed attempt must not close it", n)
			}
		})
	}
}

func TestRetryAfterSending(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		body    string
		retries bool
	}{
		{"GET without body", http.MethodGet, "", true},
		{"DELETE without body", http.MethodDelete, "", true},
		{"POST", http.MethodPost, "payload", false},
		{"PUT with body", http.MethodPut, "payload", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, b := retryPool(t, 2, Health{}, "a", "b")
			b.sendFail["a"] = true
			var body *clientBody
			if tt.body != "" {
				body = &clientBody{Reader: strings.NewReader(tt.body)}
			}
			_, err := roundTrip(p, context.Background(), tt.method, body)
			if tt.retries {
				assertHosts(t, b, "a", "b")
				if err != nil {
					t.Errorf("err = %v, want the retry to succeed", err)
				}
			} else {
				assertHosts(t, b, "a")
				if err == nil {
					t.Error("a request the upstream may have processed was retried")
				}
			}
		})
	}
}

func TestNoRetryOnceBodyWasRead(t *testing.T) {
	p, b := retryPool(t, 2, Health{}, "a", "b")
	b.probe["a"] = true
	roundTrip(p, context.Background(), http.MethodPost, &clientBody{Reader: strings.NewReader("payload")})
	assertHosts(t, b, "a")
}

func TestNoRetryOnResponse(t *testing.T) {
	p, b := retryPool(t, 2, Health{}, "a", "b")
	b.status["a"] = http.StatusInternalServerError
	res, err := roundTrip(p, context.Background(), http.MethodGet, nil)
	if err != nil || res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got %v, %v; want the 500 passed through", res, err)
	}
	assertHosts(t, b, "a")
}

func TestNoRetryWhenClientCancelled(t *testing.T) {
	p, b := retryPool(t, 2, Health{}, "a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	roundTrip(p, ctx, http.MethodGet, nil)
	assertHosts(t, b, "a")
}

func TestAttempts(t *testing.T) {
	tests := []struct {
		attempts int
		want     []string
	}{
		{1, []string{"a"}},
		{2, []string{"a", "b"}},
		{5, []string{"a", "b", "c"}}, // capped: each endpoint once
	}
	for _, tt := range tests {
		p, b := retryPool(t, tt.attempts, Health{}, "a", "b", "c")
		for _, h := range []string{"a", "b", "c"} {
			b.fail[h] = true
		}
		if _, err := roundTrip(p, context.Background(), http.MethodGet, nil); !errors.Is(err, errRefused) {
			t.Errorf("attempts %d: err = %v, want the last attempt's error", tt.attempts, err)
		}
		assertHosts(t, b, tt.want...)
	}
}

func TestRetrySkipsTriedEndpoint(t *testing.T) {
	p, b := retryPool(t, 2, Health{MaxFailures: 3}, "a", "b") // a stays healthy after one failure
	b.fail["a"] = true
	roundTrip(p, context.Background(), http.MethodGet, nil)
	assertHosts(t, b, "a", "b")
}

func TestFinalFailureClosesClientBody(t *testing.T) {
	p, b := retryPool(t, 2, Health{}, "a", "b")
	b.fail["a"], b.fail["b"] = true, true
	body := &clientBody{Reader: strings.NewReader("payload")}
	roundTrip(p, context.Background(), http.MethodPost, body)
	if n := body.closes.Load(); n != 1 {
		t.Errorf("client body closed %d times, want once after the last attempt failed", n)
	}
}

func TestDefaultAttemptsIsTwo(t *testing.T) {
	p, b, _ := newTestPool(t, "failover", Health{}, "a", "b")
	p.attempts = NewPool(nil, "", Health{}, 0).attempts
	b.fail["a"] = true
	if _, err := roundTrip(p, context.Background(), http.MethodGet, nil); err != nil {
		t.Errorf("err = %v: by default one retry should reach b", err)
	}
}
