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
// names: Accept returns them as TLS server connections presenting a minted leaf.
// The rest pass through to where they were going. Connections come from a socket
// (NewListener) or from a front end calling Handle (New).
type Listener struct {
	ln     net.Listener // nil when connections arrive only through Handle
	bumps  func(name string) bool
	tls    *tls.Config // nil: nothing is bumped
	bumped chan net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	dial   dialFunc
}

// dialFunc opens a passthrough connection to its destination.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// New returns a Listener fed only through Handle. With a nil ca nothing is bumped.
func New(ca *bump.CA, bumps func(name string) bool) *Listener {
	return newListener(nil, ca, bumps, defaultDial)
}

// NewListener intercepts ln's connections, bumping the names bumps reports with
// leaves signed by ca.
func NewListener(ln net.Listener, ca *bump.CA, bumps func(name string) bool) *Listener {
	return newListener(ln, ca, bumps, defaultDial)
}

var defaultDial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext

func newListener(ln net.Listener, ca *bump.CA, bumps func(name string) bool, dial dialFunc) *Listener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{ln: ln, bumps: bumps, bumped: make(chan net.Conn), ctx: ctx, cancel: cancel, dial: dial}
	if ca != nil {
		l.tls = ca.TLSConfig(bumps)
	}
	if ln != nil {
		go l.acceptLoop()
	}
	return l
}

func (l *Listener) acceptLoop() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			l.cancel()
			return
		}
		go l.decide(c, "")
	}
}

// Handle intercepts conn, which a front end (CONNECT, a transparent redirect)
// knows was headed for dst: bumped, it comes out of Accept; otherwise it passes
// through to dst. It returns once conn is closed. Logs and requests name
// conn.RemoteAddr() as the client, so a front end that knows the original source
// wraps conn to report it.
func (l *Listener) Handle(conn net.Conn, dst string) { l.decide(conn, dst) }

// decide bumps c when we want it and passes it through to dst otherwise.
func (l *Listener) decide(c net.Conn, dst string) {
	h, replay, err := readHello(c, helloTimeout)
	if err != nil {
		c.Close()
		return
	}
	if !l.wants(h) {
		l.passThrough(replay, l.destination(c, dst, h.sni), h.sni)
		return
	}
	slog.Info("connection", "mode", "bump", "client", c.RemoteAddr().String(), "sni", h.sni)
	select {
	case l.bumped <- tls.Server(replay, l.tls):
		<-replay.closed
	case <-l.ctx.Done():
		c.Close()
	}
}

// wants reports whether to bump a client that sent h: a configured name, offering
// HTTP (no ALPN at all means HTTP/1.1).
func (l *Listener) wants(h hello) bool {
	offersHTTP := len(h.protos) == 0 || slices.Contains(h.protos, "h2") || slices.Contains(h.protos, "http/1.1")
	return l.tls != nil && h.sni != "" && offersHTTP && l.bumps(h.sni)
}

// redirected is a connection that knows where its client was going, such as one
// whose PROXY protocol header said so.
type redirected interface{ Destination() string }

// destination is where to pass c through to: dst from a front end, else where c
// says its client was going, else for a direct connection the SNI on our own
// port: the client resolved that name to us, so it meant that port on the real host.
func (l *Listener) destination(c net.Conn, dst, sni string) string {
	if dst != "" {
		return dst
	}
	if r, ok := c.(redirected); ok && r.Destination() != "" {
		return r.Destination()
	}
	if sni == "" {
		return ""
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return net.JoinHostPort(sni, port)
}

// Accept returns the next bumped connection; it has not done its handshake yet.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.bumped:
		return c, nil
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close stops Accept and closes the socket, if any.
func (l *Listener) Close() error {
	l.cancel()
	if l.ln == nil {
		return nil
	}
	return l.ln.Close()
}

func (l *Listener) Addr() net.Addr {
	if l.ln == nil {
		return &net.TCPAddr{}
	}
	return l.ln.Addr()
}
