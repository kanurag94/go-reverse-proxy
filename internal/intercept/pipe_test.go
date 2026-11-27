package intercept

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// ends returns a connection pipe() sees and the far end a test drives.
func ends(t *testing.T) (near, far net.Conn) {
	t.Helper()
	near, far = net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	return near, far
}

// run starts pipe and returns a channel with its counts once it returns.
func run(client, up net.Conn) <-chan [2]int64 {
	done := make(chan [2]int64, 1)
	go func() { s, r := pipe(client, up); done <- [2]int64{s, r} }()
	return done
}

func wait(t *testing.T, done <-chan [2]int64) [2]int64 {
	t.Helper()
	select {
	case n := <-done:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not return")
		return [2]int64{}
	}
}

func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	buf := make([]byte, n)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func TestPipeCopiesBothWaysAndCounts(t *testing.T) {
	clientNear, client := ends(t)
	upNear, up := ends(t)
	done := run(clientNear, upNear)

	go io.WriteString(client, "hello")
	if got := readN(t, up, 5); got != "hello" {
		t.Errorf("upstream got %q, want hello", got)
	}
	go io.WriteString(up, "world!")
	if got := readN(t, client, 6); got != "world!" {
		t.Errorf("client got %q, want world!", got)
	}
	up.Close() // the upstream is done
	if n := wait(t, done); n != [2]int64{5, 6} {
		t.Errorf("sent, received = %v, want 5, 6", n)
	}
}

func TestPipeClosesTheOtherSide(t *testing.T) {
	clientNear, client := ends(t)
	upNear, up := ends(t)
	done := run(clientNear, upNear)

	client.Close() // the client leaves
	up.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := up.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("upstream read: %v; want EOF, its side closed too", err)
	}
	wait(t, done)
}
