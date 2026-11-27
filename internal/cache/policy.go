package cache

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Statuses a cache may store when the response gives a lifetime (RFC 9110 §15.1).
var cacheableStatus = map[int]bool{
	200: true, 203: true, 204: true, 300: true, 301: true, 308: true,
	404: true, 405: true, 410: true, 414: true, 501: true,
}

// policy decides what a shared cache may store. privateHeaders are the request
// headers that identify a user; their responses are private unless marked shared.
type policy struct {
	privateHeaders []string
}

func newPolicy(extra []string) policy {
	private := []string{"Authorization", "Cookie"}
	for _, h := range extra {
		private = append(private, http.CanonicalHeaderKey(h))
	}
	return policy{privateHeaders: private}
}

// requestPolicy reports whether a stored response may answer r, and whether
// the response to r may be stored. A HEAD may be answered from a stored GET,
// but its own response has no body to store.
func requestPolicy(r *http.Request) (use, store bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead || r.Header.Get("Upgrade") != "" || r.Header.Get("Range") != "" {
		return false, false
	}
	for _, h := range []string{"If-Match", "If-Unmodified-Since", "If-Range"} {
		if r.Header.Get(h) != "" {
			return false, false // preconditions guard writes and ranges; the upstream decides
		}
	}
	store = r.Method == http.MethodGet
	cc := parseCacheControl(r.Header["Cache-Control"])
	if len(cc) == 0 {
		cc = parseCacheControl(r.Header["Pragma"]) // HTTP/1.0 clients; ignored when Cache-Control is present
	}
	if has(cc, "no-store") {
		return false, false
	}
	if has(cc, "no-cache") || cc["max-age"] == "0" {
		return false, store
	}
	return true, store
}

// unchanged reports whether a client sending req already has the response with
// header stored: by its If-None-Match, or, only without one, its If-Modified-Since
// (RFC 9110 §13.1.2, §13.1.3, §13.2.2).
func unchanged(req, stored http.Header) bool {
	if lines := req["If-None-Match"]; len(lines) > 0 {
		return tagListMatches(lines, stored.Get("ETag"))
	}
	since, errSince := http.ParseTime(req.Get("If-Modified-Since"))
	modified, errModified := http.ParseTime(stored.Get("Last-Modified"))
	return errSince == nil && errModified == nil && !modified.After(since)
}

// tagListMatches reports whether If-None-Match values name etag, comparing weakly
// as If-None-Match does, or are "*".
func tagListMatches(lines []string, etag string) bool {
	for _, line := range lines {
		for tag := range strings.SplitSeq(line, ",") {
			tag = strings.TrimSpace(tag)
			if tag == "*" || etag != "" && strings.TrimPrefix(tag, "W/") == strings.TrimPrefix(etag, "W/") {
				return true
			}
		}
	}
	return false
}

// freshFor returns how long the response stays fresh; zero means do not store it.
func (p policy) freshFor(req http.Header, status int, h http.Header, now time.Time) time.Duration {
	cc := parseCacheControl(h["Cache-Control"])
	if !cacheableStatus[status] || has(cc, "no-store", "private", "no-cache") ||
		len(h["Set-Cookie"]) > 0 || len(h["Trailer"]) > 0 || slices.Contains(varyNames(h), "*") {
		return 0
	}
	identified := slices.ContainsFunc(p.privateHeaders, func(k string) bool { _, ok := req[k]; return ok })
	if identified && !has(cc, "public", "s-maxage", "must-revalidate") {
		return 0 // RFC 9111 §3.5: never share one user's response with another
	}
	return max(lifetime(cc, h, now)-initialAge(h), 0)
}

func lifetime(cc map[string]string, h http.Header, now time.Time) time.Duration {
	for _, d := range []string{"s-maxage", "max-age"} {
		if v, ok := cc[d]; ok {
			return deltaSeconds(v)
		}
	}
	expires, err := http.ParseTime(h.Get("Expires"))
	if err != nil {
		return 0 // absent, or invalid, which RFC 9111 §5.3 says means already expired
	}
	date, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		date = now
	}
	return expires.Sub(date)
}

func initialAge(h http.Header) time.Duration {
	return deltaSeconds(h.Get("Age"))
}

// deltaSeconds parses a count of seconds, capped at 2^31 (RFC 9111 §1.2.2).
// Anything but digits gives 0.
func deltaSeconds(v string) time.Duration {
	if v == "" || strings.Trim(v, "0123456789") != "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n > 1<<31 {
		n = 1 << 31 // only digits, so err can only be overflow
	}
	return time.Duration(n) * time.Second
}

func parseCacheControl(lines []string) map[string]string {
	cc := make(map[string]string)
	for _, line := range lines {
		for part := range strings.SplitSeq(line, ",") {
			name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name != "" {
				cc[strings.ToLower(name)] = strings.Trim(value, `"`)
			}
		}
	}
	return cc
}

func varyNames(h http.Header) []string {
	var names []string
	for _, line := range h["Vary"] {
		for name := range strings.SplitSeq(line, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, http.CanonicalHeaderKey(name))
			}
		}
	}
	return names
}

func has(cc map[string]string, names ...string) bool {
	return slices.ContainsFunc(names, func(n string) bool { _, ok := cc[n]; return ok })
}
