package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/accesslog"
	"github.com/kanurag94/go-reverse-proxy/internal/bump"
	"github.com/kanurag94/go-reverse-proxy/internal/cache"
	"github.com/kanurag94/go-reverse-proxy/internal/connect"
	"github.com/kanurag94/go-reverse-proxy/internal/intercept"
	"github.com/kanurag94/go-reverse-proxy/internal/proxyproto"
	"github.com/kanurag94/go-reverse-proxy/internal/router"
	"github.com/kanurag94/go-reverse-proxy/internal/watch"
	"github.com/kanurag94/go-reverse-proxy/pkg/config"
)

func main() {
	path := flag.String("config", "config.json", "path to the JSON config")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *path); err != nil {
		slog.Error("proxy stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	store := cache.NewStore(cfg.CacheLimits()) // one store for the process, shared by every snapshot
	snap, err := router.Build(cfg, store)
	if err != nil {
		return err
	}
	live := router.NewLive(snap)
	stopWatch, err := watch.File(path, func(content []byte) {
		if err := reload(content, store, live); err != nil {
			slog.Error("config reload rejected, keeping the current one", "path", path, "err", err)
			return
		}
		slog.Info("config reloaded", "path", path)
	})
	if err != nil {
		return err
	}
	defer stopWatch()

	var servers []*http.Server
	errs := make(chan error, 2*len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		h, err := live.Handler(l.Name)
		if err != nil {
			return err
		}
		srv := httpServer(accesslog.New(h))
		ln, err := net.Listen("tcp", l.Address)
		if err != nil {
			return err
		}
		sockets, err := listen(l, ln, live, srv)
		if err != nil {
			return fmt.Errorf("listener %s: %w", l.Name, err)
		}
		servers = append(servers, srv)
		slog.Info("listening", "listener", l.Name, "address", ln.Addr().String())
		for _, s := range sockets {
			go func() { errs <- fmt.Errorf("listener %s: %w", l.Name, srv.Serve(s)) }()
		}
	}
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		err := shutdown(servers, 30*time.Second)
		live.Close()
		return err
	}
}

// listen returns the sockets srv serves for l: ln itself, ln wrapped for TLS or
// bumping, or for a CONNECT listener ln plus the tunnels it bumps.
func listen(l config.Listener, ln net.Listener, live *router.Live, srv *http.Server) ([]net.Listener, error) {
	ca, err := loadCA(l)
	if err != nil {
		return nil, err
	}
	bumps := func(name string) bool { return live.HasDomain(l.Name, name) }
	if l.ProxyProtocol {
		ln = proxyproto.NewListener(ln)
	}
	switch {
	case l.Connect:
		tunnels := intercept.New(ca, bumps)
		srv.Handler = connect.Handler(srv.Handler, tunnels)
		return []net.Listener{ln, tunnels}, nil
	case ca != nil:
		return []net.Listener{intercept.NewListener(ln, ca, bumps)}, nil
	case len(l.TLS) > 0:
		cfg, err := tlsConfig(l.TLS)
		if err != nil {
			return nil, err
		}
		return []net.Listener{tls.NewListener(ln, cfg)}, nil
	}
	return []net.Listener{ln}, nil
}

// httpServer serves h over HTTP/1, h2 (on TLS connections) and h2c.
func httpServer(h http.Handler) *http.Server {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	p.SetUnencryptedHTTP2(true)
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, Protocols: p}
}

// loadCA loads the CA of a bumping listener, creating it on first use; nil when l does not bump.
func loadCA(l config.Listener) (*bump.CA, error) {
	if l.BumpDir() == "" {
		return nil, nil
	}
	ca, err := bump.LoadOrCreate(l.BumpDir())
	if err != nil {
		return nil, fmt.Errorf("bump: %w", err)
	}
	slog.Info("bumping TLS; clients must trust the CA", "listener", l.Name, "ca", filepath.Join(l.BumpDir(), "ca.pem"))
	return ca, nil
}

// tlsConfig loads a listener's certificates. With several, crypto/tls picks the one
// matching the client's SNI, falling back to the first.
func tlsConfig(certs []config.Cert) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	for i, c := range certs {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls %d: %w", i, err)
		}
		cfg.Certificates = append(cfg.Certificates, cert)
	}
	return cfg, nil
}

// reload swaps in the config in content. On any error the current one keeps serving.
func reload(content []byte, store *cache.Store, live *router.Live) error {
	cfg, err := config.Parse(content)
	if err != nil {
		return err
	}
	snap, err := router.Build(cfg, store)
	if err != nil {
		return err
	}
	return live.Swap(snap)
}

// shutdown stops every server from accepting connections and waits up to grace
// for in-flight requests. All at once: one at a time, the others would keep
// accepting while the first drains.
func shutdown(servers []*http.Server, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Go(func() { errs[i] = s.Shutdown(ctx) })
	}
	wg.Wait()
	return errors.Join(errs...)
}
