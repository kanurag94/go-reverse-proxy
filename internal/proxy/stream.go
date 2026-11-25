package proxy

import (
	"io"
	"mime"
	"net/http"
)

func copyBody(w http.ResponseWriter, res *http.Response) {
	rc := http.NewResponseController(w)
	stream := isStream(res)
	if stream {
		rc.Flush() // send headers now; a client may wait long for the first event
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // the client is gone
			}
			if stream {
				rc.Flush()
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			// Headers are sent, so a 502 is impossible. Abort, so the client
			// sees a broken response instead of a complete-looking one.
			panic(http.ErrAbortHandler)
		}
	}
}

func isStream(res *http.Response) bool {
	ct, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	return ct == "text/event-stream" || res.ContentLength == -1
}
