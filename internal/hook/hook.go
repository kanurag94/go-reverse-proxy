// Package hook runs callbacks at each phase of a proxied exchange, as Envoy's
// external processor (ext_proc) does, but in process.
package hook

import (
	"bytes"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// Hook decides per request whether to process it; nil skips the request.
type Hook func(r *http.Request) *Processor

// Processor holds one request's callbacks, one per ext_proc phase; a nil func
// skips that phase. Request phases run in hook order, response phases in reverse,
// as nested middleware would. Headers and trailers are edited in place, and a
// returned ImmediateResponse ends the exchange.
type Processor struct {
	OnRequestHeaders func(h http.Header) *ImmediateResponse
	// OnRequestBody gets the whole body, buffered, and returns the body to send.
	OnRequestBody func(body []byte) ([]byte, *ImmediateResponse)
	// OnRequestTrailers runs once the body is buffered, the only time they exist.
	OnRequestTrailers func(t http.Header) *ImmediateResponse
	OnResponseHeaders func(status int, h http.Header) *ImmediateResponse
	// OnResponseBody gets each chunk as it is written, then (nil, true) once at the
	// end, and returns the bytes to send on. chunk is only valid during the call.
	OnResponseBody func(chunk []byte, endOfStream bool) []byte
	// OnResponseTrailers may edit, remove or add trailers; it is too late to answer.
	OnResponseTrailers func(t http.Header)
}

// ImmediateResponse is sent in place of the upstream's response, as in ext_proc.
type ImmediateResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// MaxBufferedBody bounds a request body buffered for OnRequestBody; a larger
// one is answered 413.
const MaxBufferedBody = 32 << 20

func New(next http.Handler, hooks ...Hook) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ps []*Processor
		for _, h := range hooks {
			if p := h(r); p != nil {
				ps = append(ps, p)
			}
		}
		if len(ps) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if ir := processRequest(ps, r); ir != nil {
			ir.send(w)
			return
		}
		rw := &responseWriter{ResponseWriter: w, processors: ps}
		next.ServeHTTP(rw, r)
		rw.finish()
	})
}

// processRequest runs the request phases. The body is buffered only when a
// processor asks for it (ext_proc's BUFFERED mode); otherwise it streams (SKIP).
func processRequest(ps []*Processor, r *http.Request) *ImmediateResponse {
	for _, p := range ps {
		if p.OnRequestHeaders != nil {
			if ir := p.OnRequestHeaders(r.Header); ir != nil {
				return ir
			}
		}
	}
	if !slices.ContainsFunc(ps, func(p *Processor) bool { return p.OnRequestBody != nil || p.OnRequestTrailers != nil }) {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBufferedBody+1))
	switch {
	case err != nil:
		return &ImmediateResponse{Status: http.StatusBadRequest}
	case len(body) > MaxBufferedBody:
		return &ImmediateResponse{Status: http.StatusRequestEntityTooLarge}
	}
	for _, p := range ps {
		if p.OnRequestBody != nil {
			var ir *ImmediateResponse
			if body, ir = p.OnRequestBody(body); ir != nil {
				return ir
			}
		}
	}
	if r.Trailer == nil {
		r.Trailer = make(http.Header) // read to the end, so trailers, if any, are in
	}
	for _, p := range ps {
		if p.OnRequestTrailers != nil {
			if ir := p.OnRequestTrailers(r.Trailer); ir != nil {
				return ir
			}
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength, r.TransferEncoding = int64(len(body)), nil // sent with its new length
	return nil
}

func (ir *ImmediateResponse) send(w http.ResponseWriter) {
	maps.Copy(w.Header(), ir.Headers)
	status := ir.Status
	if status == 0 {
		status = http.StatusInternalServerError // WriteHeader(0) would panic
	}
	w.WriteHeader(status)
	w.Write(ir.Body)
}

// responseWriter runs the response phases on what next writes.
type responseWriter struct {
	http.ResponseWriter
	processors       []*Processor
	headersProcessed bool
	sentImmediate    bool // the upstream's response is dropped
}

func (w *responseWriter) WriteHeader(code int) {
	if w.headersProcessed {
		return
	}
	w.headersProcessed = true
	h := w.Header()
	for _, p := range slices.Backward(w.processors) {
		if p.OnResponseHeaders == nil {
			continue
		}
		if ir := p.OnResponseHeaders(code, h); ir != nil {
			w.sentImmediate = true
			clear(h) // the upstream's Content-Length and the like no longer apply
			ir.send(w.ResponseWriter)
			return
		}
	}
	if slices.ContainsFunc(w.processors, func(p *Processor) bool { return p.OnResponseBody != nil }) {
		h.Del("Content-Length") // a body hook may change the length
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK) // a no-op once the headers are processed
	if w.sentImmediate {
		return len(b), nil
	}
	if _, err := w.ResponseWriter.Write(w.body(b, false)); err != nil {
		return 0, err
	}
	return len(b), nil // what the caller gave us, whatever the hooks made of it
}

func (w *responseWriter) body(b []byte, endOfStream bool) []byte {
	for _, p := range slices.Backward(w.processors) {
		if p.OnResponseBody != nil {
			b = p.OnResponseBody(b, endOfStream)
		}
	}
	return b
}

// finish ends the body and processes the trailers, which next sets with
// http.TrailerPrefix as proxy.Handler does; net/http sends them after we return.
func (w *responseWriter) finish() {
	w.WriteHeader(http.StatusOK)
	if w.sentImmediate {
		return
	}
	if b := w.body(nil, true); len(b) > 0 {
		w.ResponseWriter.Write(b)
	}
	h, t := w.Header(), make(http.Header)
	for k, vs := range h {
		if name, ok := strings.CutPrefix(k, http.TrailerPrefix); ok {
			t[name] = vs
			delete(h, k)
		}
	}
	for _, p := range slices.Backward(w.processors) {
		if p.OnResponseTrailers != nil {
			p.OnResponseTrailers(t)
		}
	}
	for k, vs := range t {
		h[http.TrailerPrefix+k] = vs
	}
}

// Unwrap lets http.ResponseController reach Flush and Hijack on the real writer.
// Nothing is buffered here, so flushing past us loses nothing.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
