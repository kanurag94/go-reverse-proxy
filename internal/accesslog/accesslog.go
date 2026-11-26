package accesslog

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"time"
)

func New(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		aborted := true
		defer func() {
			status := sw.status
			if status == 0 {
				status = http.StatusOK // what net/http sends when the handler sets none
			}
			args := []any{"method", r.Method, "host", r.Host, "path", r.URL.EscapedPath(),
				"status", status, "bytes", sw.bytes, "duration", time.Since(start), "aborted", aborted}
			if r.TLS != nil {
				args = append(args, "sni", r.TLS.ServerName, "alpn", r.TLS.NegotiatedProtocol)
			}
			slog.InfoContext(r.Context(), "request", args...)
		}()
		next.ServeHTTP(sw, r)
		aborted = false
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.status == 0 {
		sw.status = code
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	n, err := sw.ResponseWriter.Write(b)
	sw.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush on the real writer.
func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }

// Hijack records 101: a handler takes over the connection to switch protocols.
func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(sw.ResponseWriter).Hijack()
	if err == nil && sw.status == 0 {
		sw.status = http.StatusSwitchingProtocols
	}
	return conn, brw, err
}
