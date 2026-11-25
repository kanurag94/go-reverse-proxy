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
	flights  *group
	upstream string
	policy   policy
	now      func() time.Time
}

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
		h.fetch(w, r, key, reason, store, nil)
		return
	}

	// 1. A fresh copy is stored: serve it.
	served, reason := h.serveFresh(w, r, key)
	if served {
		return
	}

	// 2. Another request is already fetching it: wait for that, then look again.
	f, leader := h.flights.join(key)
	if !leader {
		f.wait(r.Context())
		if served, reason = h.serveFresh(w, r, key); !served {
			h.fetch(w, r, key, reason, true, nil)
		}
		return
	}

	// 3. Fetch it ourselves; requests that arrive meanwhile wait for us.
	release := func() { h.flights.release(key, f) }
	defer release() // after fetch has stored the response, and also if the upstream aborts
	h.fetch(w, r, key, reason, true, release)
}

// serveFresh answers from the store if it holds a fresh copy for r,
// or returns the Cache-Status reason it could not.
func (h *Handler) serveFresh(w http.ResponseWriter, r *http.Request, key string) (bool, string) {
	now := h.now()
	e, reason := h.lookup(key, r, now)
	if e == nil {
		return false, reason
	}
	serveHit(w, e, now)
	return true, ""
}

// fetch forwards r upstream, streaming the response to the client while keeping
// a copy, and stores it if it may. onGiveUp is told once it will not be stored.
func (h *Handler) fetch(w http.ResponseWriter, r *http.Request, key, reason string, store bool, onGiveUp func()) {
	now := h.now() // before forwarding: an Age counted from here never under-reports
	c := &capture{ResponseWriter: w, cacheStatus: name + "; fwd=" + reason, limit: h.store.maxObject, onGiveUp: onGiveUp}
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
