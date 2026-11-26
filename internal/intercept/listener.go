package intercept

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"slices"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/bump"
)

const helloTimeout = 10 * time.Second // how long a client may take to send its ClientHello

// Listener reads each connection's ClientHello and bumps the ones for configured
// names: Accept returns them as TLS server connections presenting a minted leaf,
// for an http.Server to serve as usual. The rest pass through, byte for byte, to
// where they were going. Closing it closes the real listener, which ends the
// accept loop and with it Accept.
type Listener struct {
	net.Listener // the real one
	bumps        func(name string) bool
	tls          *tls.Config
	bumped       chan net.Conn
	ctx          context.Context // done once the real listener is closed
	dial         dialFunc
}

// dialFunc opens a passthrough connection to its destination.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func NewListener(ln net.Listener, ca *bump.CA, bumps func(name string) bool) *Listener {
	return newListener(ln, ca, bumps, (&net.Dialer{Timeout: 5 * time.Second}).DialContext)
}

func newListener(ln net.Listener, ca *bump.CA, bumps func(name string) bool, dial dialFunc) *Listener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{Listener: ln, bumps: bumps, tls: ca.TLSConfig(bumps), bumped: make(chan net.Conn), ctx: ctx, dial: dial}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				cancel()
				return
			}
			go l.decide(c, "") // a direct connection: no front end says where it was going
		}
	}()
	return l
}

// Handle intercepts conn, which a front end (CONNECT, a transparent redirect)
// knows was headed for dst: bumped, it comes out of Accept; otherwise it passes
// through to dst. Logs and requests name conn.RemoteAddr() as the client, so a
// front end that knows the original source wraps conn to report it.
func (l *Listener) Handle(conn net.Conn, dst string) { l.decide(conn, dst) }

// decide bumps c when we want it and passes it through to dst otherwise.
func (l *Listener) decide(c net.Conn, dst string) {
	h, replay, err := readHello(c, helloTimeout)
	if err != nil {
		c.Close()
		return
	}
	if !l.wants(h) {
		l.passThrough(replay, l.destination(dst, h.sni), h.sni)
		return
	}
	slog.Info("connection", "mode", "bump", "client", c.RemoteAddr().String(), "sni", h.sni)
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

// destination is dst, or for a direct connection the SNI on our own port: the
// client resolved that name to us, so it meant that port on the real host.
func (l *Listener) destination(dst, sni string) string {
	if dst != "" || sni == "" {
		return dst
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return net.JoinHostPort(sni, port)
}

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.bumped:
		return c, nil
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}
