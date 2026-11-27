package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

const (
	local = 0x20 // version 2, LOCAL
	proxy = 0x21 // version 2, PROXY
)

// header encodes a PROXY protocol v2 header for TCP from src to dst.
func header(cmd byte, src, dst *net.TCPAddr) []byte {
	var b bytes.Buffer
	b.WriteString("\r\n\r\n\x00\r\nQUIT\n")
	b.WriteByte(cmd)
	var addrs []byte
	if s4, d4 := src.IP.To4(), dst.IP.To4(); s4 != nil && d4 != nil {
		b.WriteByte(0x11)
		addrs = append(append(addrs, s4...), d4...)
	} else {
		b.WriteByte(0x21)
		addrs = append(append(addrs, src.IP.To16()...), dst.IP.To16()...)
	}
	addrs = binary.BigEndian.AppendUint16(addrs, uint16(src.Port))
	addrs = binary.BigEndian.AppendUint16(addrs, uint16(dst.Port))
	addrs = append(addrs, 0x04, 0x00, 0x01, 0xff) // a TLV, which is skipped
	binary.Write(&b, binary.BigEndian, uint16(len(addrs)))
	b.Write(addrs)
	return b.Bytes()
}

func tcp(s string) *net.TCPAddr {
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		panic(err)
	}
	return a
}

func listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := NewListener(raw)
	t.Cleanup(func() { ln.Close() })
	return ln, raw.Addr().String()
}

func send(t *testing.T, addr string, b []byte) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
	return c
}

func accept(t *testing.T, ln net.Listener) *Conn {
	t.Helper()
	got := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			got <- c
		}
	}()
	select {
	case c := <-got:
		t.Cleanup(func() { c.Close() })
		return c.(*Conn)
	case <-time.After(5 * time.Second):
		t.Fatal("nothing accepted")
		return nil
	}
}

func TestHeaderNamesClientAndDestination(t *testing.T) {
	for _, tc := range []struct{ src, dst string }{
		{"10.1.2.3:51000", "160.79.104.10:443"},
		{"[fd00::1]:51000", "[2607:6bc0::10]:443"},
	} {
		ln, addr := listen(t)
		send(t, addr, append(header(proxy, tcp(tc.src), tcp(tc.dst)), "hello"...))
		c := accept(t, ln)
		if c.RemoteAddr().String() != tc.src || c.Destination() != tc.dst {
			t.Errorf("client %s, destination %q; want %s, %s", c.RemoteAddr(), c.Destination(), tc.src, tc.dst)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
			t.Errorf("read %q, %v after the header; want the bytes that followed it", buf, err)
		}
	}
}

// LOCAL is the sender's own connection, a health check: the socket's addresses
// stand and there is no destination.
func TestLocalKeepsTheSocketAddresses(t *testing.T) {
	ln, addr := listen(t)
	client := send(t, addr, header(local, tcp("10.1.2.3:1"), tcp("10.1.2.4:2")))
	c := accept(t, ln)
	if c.RemoteAddr().String() != client.LocalAddr().String() || c.Destination() != "" {
		t.Errorf("client %s, destination %q; want %s and none", c.RemoteAddr(), c.Destination(), client.LocalAddr())
	}
}

// A connection without a valid header is closed, and the ones after it are
// still accepted.
func TestConnectionWithoutHeaderIsClosed(t *testing.T) {
	for name, b := range map[string][]byte{
		"not PROXY":   []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		"version 1":   []byte("PROXY TCP4 10.1.2.3 10.1.2.4 1 2\r\n"),
		"UDP":         append(header(proxy, tcp("10.1.2.3:1"), tcp("10.1.2.4:2"))[:13:13], 0x12, 0, 12),
		"short block": append(header(proxy, tcp("10.1.2.3:1"), tcp("10.1.2.4:2"))[:13:13], 0x11, 0, 4, 1, 2, 3, 4),
	} {
		ln, addr := listen(t)
		bad := send(t, addr, b)
		send(t, addr, header(proxy, tcp("10.1.2.3:1"), tcp("10.1.2.4:2")))
		if c := accept(t, ln); c.Destination() != "10.1.2.4:2" {
			t.Errorf("%s: accepted %s, want the good connection after it", name, c.RemoteAddr())
		}
		bad.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := bad.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("%s: the connection was not closed (%v)", name, err)
		}
	}
}

// A client that never sends its header must not hold up the others.
func TestSilentClientDoesNotBlockAccept(t *testing.T) {
	ln, addr := listen(t)
	send(t, addr, nil)
	send(t, addr, header(proxy, tcp("10.1.2.3:1"), tcp("10.1.2.4:2")))
	if c := accept(t, ln); c.Destination() != "10.1.2.4:2" {
		t.Errorf("accepted %q", c.Destination())
	}
}

func TestCloseEndsAccept(t *testing.T) {
	ln, _ := listen(t)
	done := make(chan error, 1)
	go func() { _, err := ln.Accept(); done <- err }()
	ln.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept after Close: %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
}
