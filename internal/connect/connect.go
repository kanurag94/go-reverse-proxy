// Package connect is a proxy for HTTPS_PROXY clients: each CONNECT tunnel is
// bumped when its host is configured and passed through otherwise.
package connect

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/bump"
	"github.com/kanurag94/go-reverse-proxy/internal/intercept"
)

// Server accepts CONNECT tunnels and serves the bumped ones with the same handler
// as plain requests.
type Server struct {
	outer, inner *http.Server
	il           *intercept.Listener
	tunnels      sync.Map // client address -> the tunnel's Proxy-Authorization
}

// NewServer bumps the hosts bumps reports with ca (nil bumps nothing) and serves
// their requests, and plain requests, with h.
func NewServer(ca *bump.CA, bumps func(host string) bool, h http.Handler) *Server {
	s := &Server{il: intercept.New(ca, bumps)}
	s.outer = &http.Server{Handler: s.connect(h), ReadHeaderTimeout: 10 * time.Second}
	s.inner = &http.Server{Handler: s.withTunnelAuth(h), ReadHeaderTimeout: 10 * time.Second}
	go s.inner.Serve(s.il)
	return s
}

func (s *Server) Serve(ln net.Listener) error { return s.outer.Serve(ln) }

func (s *Server) Shutdown(ctx context.Context) error {
	return errors.Join(s.outer.Shutdown(ctx), s.inner.Shutdown(ctx))
}

// connect hands each CONNECT tunnel to the intercepting listener; h serves the rest.
func (s *Server) connect(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			h.ServeHTTP(w, r)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, "CONNECT needs HTTP/1.1", http.StatusHTTPVersionNotSupported)
			return
		}
		brw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if brw.Flush() != nil {
			conn.Close()
			return
		}
		client := conn.RemoteAddr().String()
		s.tunnels.Store(client, r.Header.Get("Proxy-Authorization"))
		defer s.tunnels.Delete(client)
		s.il.Handle(&tunnel{Conn: conn, r: brw.Reader}, r.Host)
	})
}

// withTunnelAuth sets each bumped request's Proxy-Authorization to its tunnel's,
// so a client cannot set its own inside the tunnel.
func (s *Server) withTunnelAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("Proxy-Authorization")
		if auth, _ := s.tunnels.Load(r.RemoteAddr); auth != nil && auth != "" {
			r.Header.Set("Proxy-Authorization", auth.(string))
		}
		h.ServeHTTP(w, r)
	})
}

// tunnel is a hijacked connection whose reads start with what was already buffered.
type tunnel struct {
	net.Conn
	r *bufio.Reader
}

func (t *tunnel) Read(p []byte) (int, error) { return t.r.Read(p) }
