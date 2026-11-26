package intercept

import (
	"io"
	"net"
	"sync"
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
