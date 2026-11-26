// Package connect turns a listener into a proxy for HTTPS_PROXY clients: each
// CONNECT tunnel goes to an intercepting listener, which bumps it when its host
// is configured and passes it through otherwise.
package connect

import (
	"bufio"
	"net"
	"net/http"
	"sync"

	"github.com/kanurag94/go-reverse-proxy/internal/intercept"
)

// Handler hands CONNECT tunnels to tunnels and serves every other request with
// next: plain requests, and the requests of bumped tunnels, which get their
// tunnel's Proxy-Authorization in place of any the client sent.
func Handler(next http.Handler, tunnels *intercept.Listener) http.Handler {
	var auth sync.Map // client address -> its tunnel's Proxy-Authorization
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			open(w, r, tunnels, &auth)
			return
		}
		r.Header.Del("Proxy-Authorization")
		if a, _ := auth.Load(r.RemoteAddr); a != nil && a != "" {
			r.Header.Set("Proxy-Authorization", a.(string))
		}
		next.ServeHTTP(w, r)
	})
}

// open answers a CONNECT request and hands the connection to tunnels, keeping
// the tunnel's credential until the connection closes.
func open(w http.ResponseWriter, r *http.Request, tunnels *intercept.Listener, auth *sync.Map) {
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
	auth.Store(client, r.Header.Get("Proxy-Authorization"))
	defer auth.Delete(client)
	tunnels.Handle(&tunnel{Conn: conn, r: brw.Reader}, r.Host)
}

// tunnel is a hijacked connection whose reads start with what was already buffered.
type tunnel struct {
	net.Conn
	r *bufio.Reader
}

func (t *tunnel) Read(p []byte) (int, error) { return t.r.Read(p) }
