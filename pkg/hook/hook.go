// Package hook defines the plugin interface: a registered ConfigFactory yields a
// FilterFactory per listener, which creates a Filter per request.
package hook

import "net/http"

// Status is returned by headers callbacks. StopAndBuffer holds the message until
// its whole body has arrived, then passes it to every filter in a single body
// call; a filter may reply locally from that call.
type Status int

const (
	Continue Status = iota
	StopAndBuffer
)

// ConfigFactory is called when a listener using the plugin starts or reloads.
type ConfigFactory interface {
	Create(listener string, config []byte) (FilterFactory, error)
}

// FilterFactory creates a Filter per request. Create may return nil to skip a
// request. OnDestroy runs when the factory's config has been replaced.
type FilterFactory interface {
	Create(h Handle) Filter
	OnDestroy()
}

// Filter processes one request. Its methods are never called concurrently.
// Request callbacks run in listener order, response callbacks in reverse.
// Headers are edited in place; body callbacks return the bytes passed to the next
// filter, nil to hold them back. Each body ends with a call where endOfStream is
// true. Chunks are valid only during the call. A filter that changes a body's
// length must remove Content-Length in the matching headers callback.
// OnStreamComplete is always called last.
type Filter interface {
	OnRequestHeaders(headers http.Header, endOfStream bool) Status
	OnRequestBody(body []byte, endOfStream bool) []byte
	OnResponseHeaders(status int, headers http.Header, endOfStream bool) Status
	OnResponseBody(body []byte, endOfStream bool) []byte
	OnStreamComplete()
}

// Handle gives a filter access to its request.
type Handle interface {
	Request() *http.Request
	// SendLocalReply answers the client directly, skipping later filters and the
	// upstream. It has no effect after response headers are sent.
	SendLocalReply(status int, headers http.Header, body []byte)
}

// EmptyFilter implements Filter by passing everything through unchanged.
type EmptyFilter struct{}

func (EmptyFilter) OnRequestHeaders(http.Header, bool) Status       { return Continue }
func (EmptyFilter) OnRequestBody(b []byte, _ bool) []byte           { return b }
func (EmptyFilter) OnResponseHeaders(int, http.Header, bool) Status { return Continue }
func (EmptyFilter) OnResponseBody(b []byte, _ bool) []byte          { return b }
func (EmptyFilter) OnStreamComplete()                               {}
