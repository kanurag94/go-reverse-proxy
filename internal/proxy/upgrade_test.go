package proxy_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upgrader is a backend that switches to protocol and then echoes every byte back.
// It sends the request headers it saw to seen, if given.
func upgrader(t *testing.T, protocol string, seen chan<- http.Header) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen <- r.Header.Clone()
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", protocol)
		brw.Flush()
		io.Copy(conn, brw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// handshake sends a raw GET with the given header lines and reads the response head.
func handshake(t *testing.T, url, headers string) (net.Conn, *bufio.Reader, *http.Response) {
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n%s\r\n", headers)
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, br, res
}

const upgradeEcho = "Connection: Upgrade\r\nUpgrade: echo\r\n"

func TestUpgradeCarriesBytesBothWays(t *testing.T) {
	front := httptest.NewServer(proxyTo(t, upgrader(t, "echo", nil)))
	defer front.Close()
	conn, br, res := handshake(t, front.URL, upgradeEcho)

	if res.StatusCode != http.StatusSwitchingProtocols || res.Header.Get("Upgrade") != "echo" ||
		res.Header.Get("Connection") != "Upgrade" {
		t.Fatalf("response = %d %v, want 101 switching to echo", res.StatusCode, res.Header)
	}
	io.WriteString(conn, "ping\n")
	if line, err := br.ReadString('\n'); line != "ping\n" {
		t.Errorf("read %q, %v; want the echo of ping", line, err)
	}
}

func TestForwardsUpgradeRequestHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	front := httptest.NewServer(proxyTo(t, upgrader(t, "echo", seen)))
	defer front.Close()
	handshake(t, front.URL, "Connection: keep-alive, Upgrade\r\nKeep-Alive: timeout=5\r\nUpgrade: echo\r\n")

	h := <-seen
	if h.Get("Connection") != "Upgrade" || h.Get("Upgrade") != "echo" {
		t.Errorf("upstream got Connection %q, Upgrade %q; want the upgrade forwarded", h.Get("Connection"), h.Get("Upgrade"))
	}
	if _, ok := h["Keep-Alive"]; ok {
		t.Error("Keep-Alive reached the upstream; other hop-by-hop headers must still be removed")
	}
}

func TestDeclinedUpgradeIsANormalResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain")
	}))
	defer backend.Close()
	front := httptest.NewServer(proxyTo(t, backend.URL))
	defer front.Close()
	_, _, res := handshake(t, front.URL, upgradeEcho)

	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(body) != "plain" {
		t.Errorf("response = %d %q, want the backend's plain 200", res.StatusCode, body)
	}
}

func TestUpgradeToAnotherProtocolIs502(t *testing.T) {
	front := httptest.NewServer(proxyTo(t, upgrader(t, "other", nil)))
	defer front.Close()
	_, _, res := handshake(t, front.URL, upgradeEcho)

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: the client asked for echo, not other", res.StatusCode)
	}
}

func TestUpstreamCloseEndsTheTunnel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, _ := http.NewResponseController(w).Hijack()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		brw.Flush()
		conn.Close()
	}))
	defer backend.Close()
	front := httptest.NewServer(proxyTo(t, backend.URL))
	defer front.Close()
	_, br, _ := handshake(t, front.URL, upgradeEcho)

	if _, err := br.ReadByte(); err != io.EOF {
		t.Errorf("read err = %v, want io.EOF once the upstream closed", err)
	}
}
