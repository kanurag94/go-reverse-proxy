package intercept

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/bump"
)

const helloTimeout = 10 * time.Second // how long a client may take to send its ClientHello

// Listener reads each connection's ClientHello and bumps the ones for configured
// names: Accept returns them as TLS server connections presenting a minted leaf,
// for an http.Server to serve as usual. The rest are closed, for now. Closing it
// closes the real listener, which ends the accept loop and with it Accept.
type Listener struct {
	net.Listener // the real one
	bumps        func(name string) bool
	tls          *tls.Config
	bumped       chan net.Conn
	ctx          context.Context // done once the real listener is closed
}

func NewListener(ln net.Listener, ca *bump.CA, bumps func(name string) bool) *Listener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{Listener: ln, bumps: bumps, tls: ca.TLSConfig(bumps), bumped: make(chan net.Conn), ctx: ctx}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				cancel()
				return
			}
			go l.decide(c) // a slow ClientHello holds up nobody else
		}
	}()
	return l
}

// decide bumps c when we want it; otherwise it closes c (passthrough comes next).
func (l *Listener) decide(c net.Conn) {
	h, replay, err := readHello(c, helloTimeout)
	if err != nil || !l.wants(h) {
		c.Close()
		return
	}
	select {
	case l.bumped <- tls.Server(replay, l.tls):
	case <-l.ctx.Done():
		c.Close()
	}
}

// wants reports whether to bump a client that sent h: a configured name, offering
// HTTP (no ALPN at all means HTTP/1.1).
func (l *Listener) wants(h hello) bool {
	offersHTTP := len(h.protos) == 0 || slices.Contains(h.protos, "h2") || slices.Contains(h.protos, "http/1.1")
	return h.sni != "" && offersHTTP && l.bumps(h.sni)
}

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.bumped:
		return c, nil
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}
