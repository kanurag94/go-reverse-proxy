package proxy

import "net/http"

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
		writeError(w, r, err)
		return
	}
	defer res.Body.Close()

	copyResponseHeader(w.Header(), res)
	for k := range res.Trailer {
		w.Header().Add("Trailer", k)
	}
	w.WriteHeader(res.StatusCode)
	copyBody(w, res)
	for k, vs := range res.Trailer {
		w.Header()[http.TrailerPrefix+k] = vs
	}
}
