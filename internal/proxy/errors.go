package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
)

// nginx's code for a client that left before the response. The client never
// sees it; logs and metrics do, and it keeps such requests out of 5xx alerts.
const statusClientClosedRequest = 499

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := errorStatus(r, err)
	slog.WarnContext(r.Context(), "upstream request failed",
		"method", r.Method, "host", r.Host, "path", r.URL.EscapedPath(), "status", status, "err", err)
	http.Error(w, http.StatusText(status), status)
}

func errorStatus(r *http.Request, err error) int {
	var ne net.Error
	switch {
	case r.Context().Err() == context.Canceled:
		return statusClientClosedRequest
	case errors.As(err, &ne) && ne.Timeout():
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}
