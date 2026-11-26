package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// upgradeType returns the protocol h asks to switch to, or "" if none (RFC 9110 §7.8).
func upgradeType(h http.Header) string {
	if !hasToken(h["Connection"], "upgrade") {
		return ""
	}
	return h.Get("Upgrade")
}

// switchProtocols hands the client's connection over to the protocol the upstream
// switched to, then copies bytes both ways until either side ends.
func switchProtocols(w http.ResponseWriter, r *http.Request, res *http.Response) {
	backend, ok := res.Body.(io.ReadWriteCloser) // what Transport gives for a 101
	want, got := upgradeType(r.Header), upgradeType(res.Header)
	if !ok || got == "" || !strings.EqualFold(got, want) {
		writeError(w, r, fmt.Errorf("upstream switched to %q, client asked for %q", got, want))
		return
	}
	defer backend.Close()
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer conn.Close()

	h := make(http.Header)
	copyResponseHeader(h, res)
	h.Set("Connection", "Upgrade")
	h.Set("Upgrade", got)
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	h.Write(brw)
	brw.WriteString("\r\n")
	if brw.Flush() != nil {
		return
	}
	// read via brw: bytes the server already buffered from the client are not lost
	tunnel(struct {
		io.Reader
		io.Writer
	}{brw, conn}, backend)
}

// tunnel copies bytes both ways until either side ends.
func tunnel(client, backend io.ReadWriter) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(backend, client); done <- struct{}{} }()
	go func() { io.Copy(client, backend); done <- struct{}{} }()
	<-done // the caller's closes then end the other copy
}
