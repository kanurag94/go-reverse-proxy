package proxy

import (
	"io"
	"net/http"
)

type Handler struct {
	upstream http.RoundTripper
}

func New(upstream http.RoundTripper) *Handler {
	return &Handler{upstream: upstream}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	prepareRequest(out)

	res, err := h.upstream.RoundTrip(out)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	copyResponseHeader(w.Header(), res)
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body)
}
