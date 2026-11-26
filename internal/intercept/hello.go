// Package intercept decides, per TCP connection, whether to bump its TLS or pass
// its bytes through to where it was going.
package intercept

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// hello is what the decision needs from a ClientHello.
type hello struct {
	sni    string   // lowercase, without a trailing dot
	protos []string // ALPN, in the client's order
}

var errProbed = errors.New("intercept: ClientHello read") // stops the probe handshake

// readHello lets crypto/tls parse conn's ClientHello on a view of conn that keeps
// what it reads and refuses to write, then stops the handshake there, so the
// client sees nothing. The returned conn replays the kept bytes first: whatever
// comes next, our own handshake or a passthrough, sees the connection from its
// first byte. Data that is not TLS gives an empty hello.
func readHello(conn net.Conn, timeout time.Duration) (hello, *replay, error) {
	var h hello
	var isTLS bool
	rec := &recorder{Conn: conn}
	conn.SetReadDeadline(time.Now().Add(timeout))
	err := tls.Server(rec, &tls.Config{GetConfigForClient: func(ch *tls.ClientHelloInfo) (*tls.Config, error) {
		h = hello{sni: strings.ToLower(strings.TrimSuffix(ch.ServerName, ".")), protos: slices.Clone(ch.SupportedProtos)}
		isTLS = true
		return nil, errProbed
	}}).Handshake()
	conn.SetReadDeadline(time.Time{})
	var notTLS tls.RecordHeaderError
	if !isTLS && !errors.As(err, &notTLS) {
		return hello{}, nil, err // silent past the deadline, or gone
	}
	return h, &replay{Conn: conn, r: io.MultiReader(bytes.NewReader(rec.buf.Bytes()), conn), closed: make(chan struct{})}, nil
}

// recorder keeps what is read from its Conn and refuses writes, so the probe
// handshake can neither answer the client nor send it an alert.
type recorder struct {
	net.Conn
	buf bytes.Buffer
}

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.buf.Write(p[:n])
	return n, err
}

func (r *recorder) Write([]byte) (int, error) {
	return 0, errors.New("intercept: the probe writes nothing")
}

// replay reads the bytes the probe kept, then the rest of its Conn. closed is
// closed when it is.
type replay struct {
	net.Conn
	r      io.Reader
	closed chan struct{}
	once   sync.Once
}

func (c *replay) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *replay) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
