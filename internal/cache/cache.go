// Package cache stores shareable upstream responses in memory (RFC 9111) and
// serves repeats from it, letting one request fetch for all its concurrent twins.
package cache

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

const name = "go-reverse-proxy" // our identifier in Cache-Status (RFC 9211)

// Handler serves cached responses for one upstream, and fetches through next otherwise.
type Handler struct {
	next     http.Handler
	store    *Store
	flights  *group
	upstream string
	policy   policy
	now      func() time.Time
}

// New caches next's responses in store under upstream's name. Responses to
// requests carrying any of privateHeaders are stored only when marked shareable.
func New(next http.Handler, store *Store, upstream string, privateHeaders []string) *Handler {
	return &Handler{
		next: next, store: store, flights: &group{inflight: make(map[string]*flight)},
		upstream: upstream, policy: newPolicy(privateHeaders), now: time.Now,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := h.key(r)
	use, store := requestPolicy(r)
	if !use {
		reason := "bypass"
		if store {
			reason = "request"
		}
		h.fetch(w, r, key, reason, store, nil, nil)
		return
	}

	// 1. A fresh copy is stored: serve it.
	served, stale, reason := h.serveFresh(w, r, key)
	if served {
		return
	}

	// 2. Another request is already fetching it: wait for that, then look again.
	f, leader := h.flights.join(key)
	if !leader {
		f.wait(r.Context())
		if served, stale, reason = h.serveFresh(w, r, key); !served {
			h.fetch(w, r, key, reason, true, stale, nil)
		}
		return
	}

	// 3. Fetch it ourselves; requests that arrive meanwhile wait for us.
	release := func() { h.flights.release(key, f) }
	defer release() // after fetch has stored the response, and also if the upstream aborts
	h.fetch(w, r, key, reason, true, stale, release)
}

// serveFresh answers from the store if it holds a fresh copy for r. Otherwise it
// returns the stale copy, if any, and the Cache-Status reason it could not.
func (h *Handler) serveFresh(w http.ResponseWriter, r *http.Request, key string) (served bool, stale *entry, reason string) {
	now := h.now()
	e, reason := h.lookup(key, r, now)
	if reason != "" {
		return false, e, reason
	}
	serveHit(w, e, now)
	return true, nil, ""
}

// fetch forwards r upstream, streaming the response to the client while keeping
// a copy, and stores it if it may. With a stale copy that has validators it asks
// only whether that copy changed. onGiveUp is told once nothing will be stored.
func (h *Handler) fetch(w http.ResponseWriter, r *http.Request, key, reason string, store bool, stale *entry, onGiveUp func()) {
	now := h.now() // before forwarding: an Age counted from here never under-reports
	c := &capture{ResponseWriter: w, cacheStatus: name + "; fwd=" + reason, limit: h.store.maxObject, onGiveUp: onGiveUp}
	if store {
		c.freshness = func(code int, hdr http.Header) time.Duration {
			return h.policy.freshFor(r.Header, code, hdr, now)
		}
	}
	if cr := conditional(r, stale); cr != nil {
		r, c.pending = cr, make(http.Header)
	}
	h.next.ServeHTTP(c, r)
	if c.notModified {
		h.refresh(w, r, key, stale, c.header, now)
		return
	}
	c.implicitHeader() // a handler that wrote nothing still sent a response

	if !isSafe(r.Method) && c.code >= 200 && c.code < 400 {
		h.store.delete(key) // RFC 9111 §4.4: a successful unsafe request may have changed the resource
	}
	if e := c.entry(now, r.Header); e != nil {
		h.store.put(key, e)
	}
}

// refresh serves stale as the 304 that revalidated it describes it, and keeps it
// for as long as that 304 allows (RFC 9111 §4.3.4).
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request, key string, stale *entry, notModified http.Header, now time.Time) {
	e := stale.updated(notModified, now)
	e.freshFor = h.policy.freshFor(r.Header, e.status, e.header, now)
	if e.freshFor > 0 {
		h.store.put(key, e)
	} else {
		h.store.delete(key)
	}
	writeEntry(w, e, now, name+"; fwd=stale; fwd-status=304")
}

// conditional returns r asking the upstream only whether e changed (RFC 9111 §4.3.1),
// or nil when there is no e or it has no validator to ask with.
func conditional(r *http.Request, e *entry) *http.Request {
	if e == nil {
		return nil
	}
	etag, modified := e.header.Get("ETag"), e.header.Get("Last-Modified")
	if etag == "" && modified == "" {
		return nil
	}
	c := r.Clone(r.Context())
	if etag != "" {
		c.Header.Set("If-None-Match", etag)
	}
	if modified != "" {
		c.Header.Set("If-Modified-Since", modified)
	}
	return c
}

// key names a response by upstream, so a reload that moves a route never serves
// another upstream's content, and by host, since one upstream can serve several.
func (h *Handler) key(r *http.Request) string {
	return h.upstream + " " + r.Host + r.URL.RequestURI()
}

// lookup returns the entry stored for r and, unless it is fresh, the Cache-Status
// reason not to serve it: a stale entry is returned, to revalidate.
func (h *Handler) lookup(key string, r *http.Request, now time.Time) (*entry, string) {
	e := h.store.get(key)
	switch {
	case e == nil:
		return nil, "uri-miss"
	case !e.matches(r.Header):
		return nil, "vary-miss"
	case now.Sub(e.storedAt) >= e.freshFor:
		return e, "stale"
	}
	return e, ""
}

// updated returns a copy of e as revalidated at now by a 304 with header h: h's
// fields replace e's, except Content-Length, which is the 304's own (RFC 9111 §3.2).
// The age restarts from the 304's.
func (e *entry) updated(h http.Header, now time.Time) *entry {
	u := *e
	u.header = e.header.Clone()
	u.header.Del("Age")
	for k, vs := range h {
		if k != "Content-Length" {
			u.header[k] = slices.Clone(vs)
		}
	}
	u.storedAt, u.initialAge = now, initialAge(h)
	return &u
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
	ttl := e.freshFor - now.Sub(e.storedAt)
	writeEntry(w, e, now, fmt.Sprintf("%s; hit; ttl=%d", name, int(ttl.Seconds())))
}

// writeEntry sends e to the client with its Age at now and cacheStatus.
func writeEntry(w http.ResponseWriter, e *entry, now time.Time, cacheStatus string) {
	h := w.Header()
	for k, vs := range e.header {
		h[k] = slices.Clone(vs) // a later layer may append; never share the stored slices
	}
	h.Set("Age", strconv.Itoa(int((e.initialAge + now.Sub(e.storedAt)).Seconds())))
	h.Add("Cache-Status", cacheStatus)
	w.WriteHeader(e.status)
	w.Write(e.body)
}
