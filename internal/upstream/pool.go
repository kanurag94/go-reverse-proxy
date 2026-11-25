package upstream

import (
	"net/http"
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
)

// Pool spreads requests over one upstream's endpoints, skipping those that recently
// could not be reached. Policy "failover" uses the first healthy endpoint in order;
// anything else rotates over the healthy ones.
type Pool struct {
	members  []*member
	failover bool
	health   Health
	next     atomic.Uint64
	now      func() time.Time
}

type member struct {
	*Endpoint
	failures     atomic.Int32
	ejectedUntil atomic.Int64 // unix nanoseconds; in the past means healthy
}

func NewPool(endpoints []*Endpoint, policy string, health Health) *Pool {
	if health.MaxFailures <= 0 {
		health.MaxFailures = defaultMaxFailures
	}
	if health.EjectDuration <= 0 {
		health.EjectDuration = defaultEjectDuration
	}
	p := &Pool{failover: policy == "failover", health: health, now: time.Now}
	for _, e := range endpoints {
		p.members = append(p.members, &member{Endpoint: e})
	}
	return p
}

func (p *Pool) RoundTrip(r *http.Request) (*http.Response, error) {
	m := p.pick()
	res, err := m.RoundTrip(r)
	p.record(m, r, err)
	return res, err
}

// pick chooses among the healthy endpoints. If none is healthy it chooses among
// all of them: trying one beats failing without an attempt.
func (p *Pool) pick() *member {
	now := p.now().UnixNano()
	healthy := make([]*member, 0, len(p.members))
	for _, m := range p.members {
		if m.ejectedUntil.Load() <= now {
			healthy = append(healthy, m)
		}
	}
	if len(healthy) == 0 {
		healthy = p.members
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
