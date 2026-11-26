package intercept

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// pipe copies bytes both ways between client and up. When either side finishes
// it closes both, so the other copy ends too (as Envoy's tcp_proxy does), and it
// returns once both copies have, with what each direction carried.
func pipe(client, up net.Conn) (sent, received int64) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { client.Close(); up.Close() }) }
	var wg sync.WaitGroup
	wg.Go(func() { sent, _ = io.Copy(up, client); closeBoth() })
	wg.Go(func() { received, _ = io.Copy(client, up); closeBoth() })
	wg.Wait()
	return sent, received
}

// passThrough sends c's bytes, its ClientHello included, to dst and dst's back,
// then logs the connection.
func (l *Listener) passThrough(c net.Conn, dst, sni string) {
	defer c.Close()
	log := slog.With("mode", "passthrough", "client", c.RemoteAddr().String(), "sni", sni, "destination", dst)
	if dst == "" {
		log.Warn("connection", "err", "no destination: no SNI, and no front end named one")
		return
	}
	up, err := l.dial(l.ctx, "tcp", dst)
	if err != nil {
		log.Warn("connection", "err", err)
		return
	}
	if up.RemoteAddr().String() == c.LocalAddr().String() {
		up.Close()
		log.Warn("connection", "err", "the destination is this proxy") // it would dial itself until out of descriptors
		return
	}
	start := time.Now()
	sent, received := pipe(c, up)
	log.Info("connection", "sent", sent, "received", received, "duration", time.Since(start))
}
