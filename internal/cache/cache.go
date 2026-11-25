package cache

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

const name = "go-reverse-proxy" // our identifier in Cache-Status (RFC 9211)

type Handler struct {
	next     http.Handler
	store    *Store
	upstream string
	policy   policy
	now      func() time.Time
}

func New(next http.Handler, store *Store, upstream string, credentials []string) *Handler {
	return &Handler{next: next, store: store, upstream: upstream, policy: newPolicy(credentials), now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := h.now() // request time: an Age counted from here never under-reports
	key := h.key(r)
	use, store := requestPolicy(r)

	fwd := "bypass"
	if store {
		fwd = "request"
	}
	if use {
		e, reason := h.lookup(key, r, now)
		if e != nil {
			serveHit(w, e, now)
			return
		}
		fwd = reason
	}

	c := &capture{ResponseWriter: w, cacheStatus: name + "; fwd=" + fwd, limit: h.store.maxObject}
	if store {
		c.freshness = func(code int, hdr http.Header) time.Duration {
			return h.policy.freshFor(r.Header, code, hdr, now)
		}
	}
	h.next.ServeHTTP(c, r)
	c.implicitHeader() // a handler that wrote nothing still sent a response

	if !isSafe(r.Method) && c.code >= 200 && c.code < 400 {
		h.store.delete(key) // RFC 9111 §4.4: a successful unsafe request may have changed the resource
	}
	if e := c.entry(now, r.Header); e != nil {
		h.store.put(key, e)
	}
}

// key names a response by upstream, so a reload that moves a route never serves
// another upstream's content, and by host, since one upstream can serve several.
func (h *Handler) key(r *http.Request) string {
	return h.upstream + " " + r.Host + r.URL.RequestURI()
}

// lookup returns a fresh entry for r, or nil and the Cache-Status reason there is none.
func (h *Handler) lookup(key string, r *http.Request, now time.Time) (*entry, string) {
	e := h.store.get(key)
	switch {
	case e == nil:
		return nil, "uri-miss"
	case !e.matches(r.Header):
		return nil, "vary-miss"
	case now.Sub(e.storedAt) >= e.freshFor:
		return nil, "stale"
	}
	return e, ""
}

// matches reports whether req sends the same values as the request this entry
// answered, for every header the response said it varies on.
func (e *entry) matches(req http.Header) bool {
	for name, want := range e.vary {
		if !slices.Equal(req[name], want) {
			return false
		}
	}
	return true
}

// varyValues records the request's values of the headers named in Vary.
func varyValues(names []string, req http.Header) http.Header {
	if len(names) == 0 {
		return nil
	}
	v := make(http.Header, len(names))
	for _, n := range names {
		v[n] = slices.Clone(req[n])
	}
	return v
}

func isSafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

func serveHit(w http.ResponseWriter, e *entry, now time.Time) {
	resident := now.Sub(e.storedAt)
	h := w.Header()
	for k, vs := range e.header {
		h[k] = slices.Clone(vs) // a later layer may append; never share the stored slices
	}
	h.Set("Age", strconv.Itoa(int((e.initialAge + resident).Seconds())))
	h.Add("Cache-Status", fmt.Sprintf("%s; hit; ttl=%d", name, int((e.freshFor-resident).Seconds())))
	w.WriteHeader(e.status)
	w.Write(e.body)
}
