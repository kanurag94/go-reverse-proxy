// Package proxyproto reads the PROXY protocol v2 header that a local redirector
// puts in front of each connection, naming the client and where it was going.
package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

const headerTimeout = 10 * time.Second // how long a connection may take to send its header

var signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

const (
	cmdLocal = 0x0 // the sender's own connection, such as a health check
	cmdProxy = 0x1
	tcp4     = 0x11
	tcp6     = 0x21
)

// Listener reads the header of each connection it accepts. It reads them in the
// background, so a client slow to send one holds up no other, and Accept
// returns the connections whose header is valid; the rest are closed.
type Listener struct {
	ln    net.Listener
	ready chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func NewListener(ln net.Listener) *Listener {
	l := &Listener{ln: ln, ready: make(chan net.Conn), done: make(chan struct{})}
	go l.acceptLoop()
	return l
}

func (l *Listener) acceptLoop() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			l.stop()
			return
		}
		go l.read(c)
	}
}

func (l *Listener) read(c net.Conn) {
	pc, err := readHeader(c)
	if err != nil {
		slog.Warn("PROXY protocol header", "from", c.RemoteAddr().String(), "err", err)
		c.Close()
		return
	}
	select {
	case l.ready <- pc:
	case <-l.done:
		c.Close()
	}
}

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ready:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close closes the socket, which ends the accept loop and with it Accept.
func (l *Listener) Close() error { return l.ln.Close() }

func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

func (l *Listener) stop() { l.once.Do(func() { close(l.done) }) }

// Conn is a connection after its header. RemoteAddr is the client the header
// names; Destination is where that client was going, "" for a LOCAL header.
type Conn struct {
	net.Conn
	client net.Addr
	dst    string
}

func (c *Conn) RemoteAddr() net.Addr { return c.client }
func (c *Conn) Destination() string  { return c.dst }

// readHeader reads exactly c's header, leaving what follows it unread.
func readHeader(c net.Conn) (*Conn, error) {
	c.SetReadDeadline(time.Now().Add(headerTimeout))
	defer c.SetReadDeadline(time.Time{})
	var fixed [16]byte
	if _, err := io.ReadFull(c, fixed[:]); err != nil {
		return nil, err
	}
	if !bytes.Equal(fixed[:12], signature) {
		return nil, errors.New("no PROXY protocol v2 signature")
	}
	if v := fixed[12] >> 4; v != 2 {
		return nil, fmt.Errorf("version %d", v)
	}
	cmd, family := fixed[12]&0x0f, fixed[13]
	size := int(binary.BigEndian.Uint16(fixed[14:]))
	var ipLen int
	switch {
	case cmd == cmdLocal:
		_, err := io.CopyN(io.Discard, c, int64(size))
		return &Conn{Conn: c, client: c.RemoteAddr()}, err
	case cmd != cmdProxy:
		return nil, fmt.Errorf("unknown command %#x", cmd)
	case family == tcp4:
		ipLen = 4
	case family == tcp6:
		ipLen = 16
	default:
		return nil, fmt.Errorf("unsupported address family %#x", family)
	}
	if size < 2*ipLen+4 {
		return nil, fmt.Errorf("address block of %d bytes is too short", size)
	}
	block := make([]byte, size) // addresses, then TLVs, which are not needed
	if _, err := io.ReadFull(c, block); err != nil {
		return nil, err
	}
	ports := block[2*ipLen:]
	src := netip.AddrPortFrom(ip(block[:ipLen]), binary.BigEndian.Uint16(ports))
	dst := netip.AddrPortFrom(ip(block[ipLen:2*ipLen]), binary.BigEndian.Uint16(ports[2:]))
	return &Conn{Conn: c, client: net.TCPAddrFromAddrPort(src), dst: dst.String()}, nil
}

func ip(b []byte) netip.Addr {
	a, _ := netip.AddrFromSlice(b)
	return a.Unmap()
}
