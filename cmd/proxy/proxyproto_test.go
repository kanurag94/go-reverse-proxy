package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/router"
	"github.com/kanurag94/go-reverse-proxy/pkg/config"
)

// proxyV2 is a PROXY protocol v2 header for TCP over IPv4 from src to dst.
func proxyV2(src, dst string) []byte {
	s, d := net.TCPAddrFromAddrPort(netip.MustParseAddrPort(src)), net.TCPAddrFromAddrPort(netip.MustParseAddrPort(dst))
	var b bytes.Buffer
	b.WriteString("\r\n\r\n\x00\r\nQUIT\n\x21\x11\x00\x0c")
	b.Write(s.IP.To4())
	b.Write(d.IP.To4())
	binary.Write(&b, binary.BigEndian, uint16(s.Port))
	binary.Write(&b, binary.BigEndian, uint16(d.Port))
	return b.Bytes()
}

// A redirector sends the client's connection after a PROXY header: the bumped
// request comes from the client the header names, not from the redirector.
func TestProxyProtocolBumpSeesTheClient(t *testing.T) {
	caDir := t.TempDir()
	l := config.Listener{Name: "redirected", Address: "127.0.0.1:0", ProxyProtocol: true, Bump: &config.Bump{CADir: caDir},
		Hosts: []config.Host{{Name: "api", Domains: []string{name}, Routes: []config.Route{{Prefix: "/", Upstream: "api"}}}}}
	u := config.Upstream{Name: "api", Endpoints: []string{"http://127.0.0.1:1"}}
	snap, err := router.Build(&config.Config{Listeners: []config.Listener{l}, Upstreams: []config.Upstream{u}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	clients := make(chan string, 1)
	addr := serveListener(t, l, router.NewLive(snap), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clients <- r.RemoteAddr
		io.WriteString(w, "ok")
	}))
	pemBytes, _ := os.ReadFile(filepath.Join(caDir, "ca.pem"))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pemBytes)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(proxyV2("10.9.8.7:51000", "160.79.104.10:443")); err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(conn, &tls.Config{ServerName: name, RootCAs: roots, NextProtos: []string{"http/1.1"}})
	req, _ := http.NewRequest("GET", "https://"+name+"/", nil)
	if err := req.Write(tc); err != nil {
		t.Fatal(err)
	}
	if got := <-clients; got != "10.9.8.7:51000" {
		t.Errorf("request from %s, want the header's client 10.9.8.7:51000", got)
	}
}
