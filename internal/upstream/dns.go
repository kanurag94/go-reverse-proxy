package upstream

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"time"
)

// Resolver looks up the addresses of a host. *net.Resolver implements it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

const lookupTimeout = 2 * time.Second

// FollowDNS gives every address the endpoints' hostnames resolve to a member of
// its own, and looks again every interval until ctx is done. When a lookup fails
// the pool keeps its members: before the first success, the endpoints as configured.
func (p *Pool) FollowDNS(ctx context.Context, r Resolver, interval time.Duration) {
	refresh := func() {
		endpoints, err := resolve(ctx, r, p.configured)
		if err != nil {
			slog.Warn("upstream DNS lookup failed; keeping current endpoints", "err", err)
			return
		}
		p.Update(endpoints)
	}
	refresh()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

// resolve returns one endpoint per address of each endpoint's hostname, in a
// stable order; endpoints given by IP address are kept as they are.
func resolve(ctx context.Context, r Resolver, endpoints []*Endpoint) ([]*Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var out []*Endpoint
	for _, e := range endpoints {
		host := e.url.Hostname()
		if net.ParseIP(host) != nil {
			out = append(out, e)
			continue
		}
		addrs, err := r.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("%s: no addresses", host)
		}
		slices.SortFunc(addrs, func(a, b net.IPAddr) int { return bytes.Compare(a.IP.To16(), b.IP.To16()) })
		for _, a := range addrs {
			out = append(out, e.At(a.IP.String()))
		}
	}
	return out, nil
}
