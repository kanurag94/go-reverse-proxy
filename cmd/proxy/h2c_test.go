package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/router"
	"github.com/kanurag94/go-reverse-proxy/pkg/config"
)

func h2cClient() *http.Client {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: p}, Timeout: 5 * time.Second}
}

// serveListener serves l as run does and returns its address.
func serveListener(t *testing.T, l config.Listener, live *router.Live, h http.Handler) string {
	t.Helper()
	srv := httpServer(h)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sockets, err := listen(l, ln, live, srv)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sockets {
		go srv.Serve(s)
	}
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func TestPlainListenerAcceptsH2CAndHTTP1(t *testing.T) {
	proto := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Proto) })
	url := "http://" + serveListener(t, config.Listener{Name: "plain"}, nil, proto)
	for want, c := range map[string]*http.Client{"HTTP/2.0": h2cClient(), "HTTP/1.1": {Timeout: 5 * time.Second}} {
		res, err := c.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(b) != want {
			t.Errorf("served over %s, want %s", b, want)
		}
	}
}

func TestH2CThroughTheProxyKeepsTrailers(t *testing.T) {
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "Grpc-Status")
		io.WriteString(w, r.Proto)
		w.Header().Set("Grpc-Status", "0")
	}))
	backend.Config.Protocols = new(http.Protocols)
	backend.Config.Protocols.SetUnencryptedHTTP2(true)
	backend.Start()
	defer backend.Close()

	l := config.Listener{Name: "grpc", Address: "127.0.0.1:0",
		Hosts: []config.Host{{Name: "svc", Domains: []string{"127.0.0.1"}, Routes: []config.Route{{Prefix: "/", Upstream: "svc"}}}}}
	cfg := &config.Config{Listeners: []config.Listener{l}, Upstreams: []config.Upstream{{Name: "svc", Endpoints: []string{backend.URL}, H2C: true}}}
	snap, err := router.Build(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := router.NewLive(snap)
	h, _ := live.Handler(l.Name)
	url := "http://" + serveListener(t, l, live, h)

	res, err := h2cClient().Get(url + "/svc.Echo/Call")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.Proto != "HTTP/2.0" || string(b) != "HTTP/2.0" || res.Trailer.Get("Grpc-Status") != "0" {
		t.Errorf("client %s, upstream %s, Grpc-Status %q; want HTTP/2.0 both ways and the trailer",
			res.Proto, b, res.Trailer.Get("Grpc-Status"))
	}
}
