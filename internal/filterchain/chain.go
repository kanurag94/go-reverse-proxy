// Package filterchain runs requests through plugin filters.
package filterchain

import (
	"bytes"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"github.com/kanurag94/go-reverse-proxy/pkg/hook"
)

// MaxBufferedBody is the largest body held for StopAndBuffer.
const MaxBufferedBody = 32 << 20

// New wraps next with a filter from each factory per request.
func New(next http.Handler, factories ...hook.FilterFactory) http.Handler {
	if len(factories) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := &stream{w: w, r: r}
		for _, f := range factories {
			if filter := f.Create(s); filter != nil {
				s.filters = append(s.filters, filter)
			}
		}
		if len(s.filters) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		defer s.complete() // runs even if next panics to abort the response
		noBody := r.Body == nil || r.Body == http.NoBody
		buffer := false
		for _, f := range s.filters {
			var st hook.Status
			s.call(func() { st = f.OnRequestHeaders(r.Header, noBody) })
			if s.replied {
				return
			}
			buffer = buffer || st == hook.StopAndBuffer
		}
		switch {
		case noBody:
		case buffer:
			if !s.bufferRequest() {
				return
			}
		default:
			if r.Header.Get("Content-Length") == "" {
				r.ContentLength = -1
			}
			r.Body = &requestBody{ReadCloser: r.Body, s: s}
		}
		rw := &responseWriter{ResponseWriter: w, s: s}
		next.ServeHTTP(rw, r)
		rw.finish()
	})
}

// stream holds one request's filters and implements hook.Handle.
type stream struct {
	w           http.ResponseWriter
	r           *http.Request
	filters     []hook.Filter
	mu          sync.Mutex // the request body is read on another goroutine
	replied     bool
	headersSent bool
}

func (s *stream) call(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

func (s *stream) Request() *http.Request { return s.r }

func (s *stream) SendLocalReply(status int, headers http.Header, body []byte) {
	if s.replied || s.headersSent {
		return
	}
	s.replied = true
	h := s.w.Header()
	clear(h)
	maps.Copy(h, headers)
	s.w.WriteHeader(status)
	s.w.Write(body)
}

// bufferRequest reads the whole body, passes it through the filters in one call
// and forwards the result with its length. It reports false if the request was answered.
func (s *stream) bufferRequest() bool {
	body, err := io.ReadAll(io.LimitReader(s.r.Body, MaxBufferedBody+1))
	switch {
	case err != nil:
		s.SendLocalReply(http.StatusBadRequest, nil, nil)
		return false
	case len(body) > MaxBufferedBody:
		s.SendLocalReply(http.StatusRequestEntityTooLarge, nil, nil)
		return false
	}
	for _, f := range s.filters {
		s.call(func() { body = f.OnRequestBody(body, true) })
		if s.replied {
			return false
		}
	}
	s.r.Body = io.NopCloser(bytes.NewReader(body))
	s.r.ContentLength, s.r.TransferEncoding = int64(len(body)), nil
	s.r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return true
}

func (s *stream) complete() {
	for _, f := range slices.Backward(s.filters) {
		s.call(f.OnStreamComplete)
	}
}

// requestBody passes the request body through the filters in order.
type requestBody struct {
	io.ReadCloser
	s       *stream
	pending []byte
	eof     bool
}

func (b *requestBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		if b.eof {
			return 0, io.EOF
		}
		n, err := b.ReadCloser.Read(p)
		if n > 0 {
			b.pending = append(b.pending, b.through(p[:n], false)...)
		}
		if err == io.EOF {
			b.eof = true
			b.pending = append(b.pending, b.through(nil, true)...)
		} else if err != nil {
			return 0, err
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *requestBody) through(chunk []byte, end bool) []byte {
	for _, f := range b.s.filters {
		b.s.call(func() { chunk = f.OnRequestBody(chunk, end) })
	}
	return chunk
}

// responseWriter passes the response through the filters in reverse. A stale
// Content-Length cannot corrupt the response: net/http rejects writes past it and
// closes the connection when fewer bytes arrive.
type responseWriter struct {
	http.ResponseWriter
	s           *stream
	headersDone bool
	noBody      bool
	buffering   bool
	tooLarge    bool
	code        int
	held        []byte
}

func (w *responseWriter) WriteHeader(code int) {
	if w.headersDone {
		return
	}
	w.headersDone = true
	w.noBody = w.Header().Get("Content-Length") == "0"
	for _, f := range slices.Backward(w.s.filters) {
		var st hook.Status
		w.s.call(func() { st = f.OnResponseHeaders(code, w.Header(), w.noBody) })
		if w.s.replied {
			return
		}
		w.buffering = w.buffering || st == hook.StopAndBuffer
	}
	if w.buffering {
		w.code = code
		return
	}
	w.s.headersSent = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	switch {
	case w.s.replied:
	case w.buffering:
		if len(w.held)+len(b) > MaxBufferedBody {
			w.tooLarge = true
		} else if !w.tooLarge {
			w.held = append(w.held, b...)
		}
	default:
		if _, err := w.ResponseWriter.Write(w.through(b, false)); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

func (w *responseWriter) through(chunk []byte, end bool) []byte {
	for _, f := range slices.Backward(w.s.filters) {
		w.s.call(func() { chunk = f.OnResponseBody(chunk, end) })
	}
	return chunk
}

func (w *responseWriter) finish() {
	w.WriteHeader(http.StatusOK)
	switch {
	case w.s.replied:
	case w.buffering:
		w.flushHeld()
	case !w.noBody:
		if tail := w.through(nil, true); len(tail) > 0 {
			w.ResponseWriter.Write(tail)
		}
	}
}

// flushHeld passes the buffered body through the filters in one call and sends
// the response with its new length, unless a filter replied or it was too large.
func (w *responseWriter) flushHeld() {
	if w.tooLarge {
		w.s.SendLocalReply(http.StatusBadGateway, nil, nil)
		return
	}
	body := w.held
	if !w.noBody {
		body = w.through(body, true)
		if w.s.replied {
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.s.headersSent = true
	w.ResponseWriter.WriteHeader(w.code)
	w.ResponseWriter.Write(body)
}

// FlushError flushes the response, except while it is held for buffering.
func (w *responseWriter) FlushError() error {
	if w.buffering {
		return nil
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach Flush and Hijack.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
