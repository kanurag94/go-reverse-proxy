package upstream

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// Health tunes passive health checking: an endpoint that cannot be reached
// MaxFailures times in a row is skipped for EjectDuration. Zero means the default.
type Health struct {
	MaxFailures   int
	EjectDuration time.Duration
}

const (
	defaultMaxFailures   = 1
	defaultEjectDuration = 10 * time.Second
	defaultAttempts      = 2
)

// Pool spreads requests over one upstream's endpoints, skipping those that recently
// could not be reached. Policy "failover" uses the first healthy endpoint in order;
// anything else rotates over the healthy ones. An attempt that cannot reach its
// endpoint is retried on another, up to attempts in all, when that is safe.
type Pool struct {
	members  []*member
	failover bool
	health   Health
	attempts int
	next     atomic.Uint64
	now      func() time.Time
}

type member struct {
	*Endpoint
	failures     atomic.Int32
	ejectedUntil atomic.Int64 // unix nanoseconds; in the past means healthy
}

func NewPool(endpoints []*Endpoint, policy string, health Health, attempts int) *Pool {
	if health.MaxFailures <= 0 {
		health.MaxFailures = defaultMaxFailures
	}
	if health.EjectDuration <= 0 {
		health.EjectDuration = defaultEjectDuration
	}
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	p := &Pool{failover: policy == "failover", health: health, attempts: attempts, now: time.Now}
	for _, e := range endpoints {
		p.members = append(p.members, &member{Endpoint: e})
	}
	return p
}

func (p *Pool) RoundTrip(r *http.Request) (*http.Response, error) {
	body := newReplayBody(r.Body)
	tried := make(map[*member]bool)
	for {
		m := p.pick(tried)
		tried[m] = true
		var sent atomic.Bool
		attempt := r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
			WroteHeaders: func() { sent.Store(true) },
		}))
		if body != nil {
			attempt.Body = body
		}
		res, err := m.RoundTrip(attempt)
		p.record(m, r, err)
		if err == nil {
			return res, nil
		}
		if len(tried) == min(p.attempts, len(p.members)) || !retryable(r, body, sent.Load()) {
			if body != nil {
				body.rc.Close() // no attempt is left to close it
			}
			return nil, err
		}
		slog.WarnContext(r.Context(), "retrying upstream request",
			"method", r.Method, "path", r.URL.EscapedPath(), "endpoint", m.url.Host, "err", err)
	}
}

// pick chooses among the healthy endpoints this request has not tried yet. If none
// is healthy it chooses among all untried ones: trying one beats failing without
// an attempt.
func (p *Pool) pick(tried map[*member]bool) *member {
	now := p.now().UnixNano()
	var healthy, untried []*member
	for _, m := range p.members {
		if tried[m] {
			continue
		}
		untried = append(untried, m)
		if m.ejectedUntil.Load() <= now {
			healthy = append(healthy, m)
		}
	}
	if len(healthy) == 0 {
		healthy = untried
	}
	if p.failover {
		return healthy[0]
	}
	return healthy[(p.next.Add(1)-1)%uint64(len(healthy))]
}

// record ejects m after MaxFailures consecutive failures to reach it. A response,
// whatever its status, resets the count; a request the client cancelled says
// nothing about the endpoint.
func (p *Pool) record(m *member, r *http.Request, err error) {
	switch {
	case err == nil:
		m.failures.Store(0)
	case r.Context().Err() == nil:
		if m.failures.Add(1) >= int32(p.health.MaxFailures) {
			m.failures.Store(0)
			m.ejectedUntil.Store(p.now().Add(p.health.EjectDuration).UnixNano())
		}
	}
}

// retryable reports whether r may be sent again after an attempt failed: never
// once the client left; with a body only while the upstream has seen none of it;
// without one also after it was sent, if the method is idempotent.
func retryable(r *http.Request, body *replayBody, sent bool) bool {
	switch {
	case r.Context().Err() != nil:
		return false
	case body != nil:
		return !sent && body.read.Load() == 0
	default:
		return !sent || idempotent(r.Method)
	}
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// replayBody lets the attempts of one request share its body. Attempts cannot
// close it, and it counts what they read: a body nobody read can be sent again.
type replayBody struct {
	rc   io.ReadCloser
	read atomic.Int64
}

func newReplayBody(rc io.ReadCloser) *replayBody {
	if rc == nil || rc == http.NoBody {
		return nil
	}
	return &replayBody{rc: rc}
}

func (b *replayBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *replayBody) Close() error { return nil }
