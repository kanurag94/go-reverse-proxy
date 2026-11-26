package proxy

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Headers that describe one connection, not the message (RFC 9110 §7.6.1).
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func prepareRequest(r *http.Request) {
	h := r.Header
	trailers := hasToken(h["Te"], "trailers")
	upgrade := upgradeType(h)
	removeHopHeaders(h)
	if trailers {
		h.Set("Te", "trailers")
	}
	if upgrade != "" {
		h.Set("Connection", "Upgrade")
		h.Set("Upgrade", upgrade)
	}

	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		appendList(h, "X-Forwarded-For", ip)
	}
	h.Set("X-Forwarded-Host", r.Host)
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	h.Set("X-Forwarded-Proto", proto)
	appendList(h, "Via", via(r.ProtoMajor, r.ProtoMinor))

	if _, ok := h["User-Agent"]; !ok {
		h.Set("User-Agent", "") // an empty value stops Transport adding Go-http-client
	}
}

func copyResponseHeader(dst http.Header, res *http.Response) {
	removeHopHeaders(res.Header)
	// Alt-Svc advertises the upstream's protocols and ports (RFC 7838), not ours:
	// forwarded, clients would try HTTP/3 on UDP against an address that has none.
	res.Header.Del("Alt-Svc")
	for k, vs := range res.Header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	appendList(dst, "Via", via(res.ProtoMajor, res.ProtoMinor))
}

func removeHopHeaders(h http.Header) {
	for _, line := range h["Connection"] {
		for name := range strings.SplitSeq(line, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

func hasToken(lines []string, token string) bool {
	for _, line := range lines {
		for t := range strings.SplitSeq(line, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func appendList(h http.Header, name, value string) {
	if prior := h[name]; len(prior) > 0 {
		value = strings.Join(prior, ", ") + ", " + value
	}
	h.Set(name, value)
}

func via(major, minor int) string {
	if major >= 2 {
		return fmt.Sprintf("%d go-reverse-proxy", major)
	}
	return fmt.Sprintf("%d.%d go-reverse-proxy", major, minor)
}
