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

	"github.com/kanurag94/go-reverse-proxy/internal/watch"
	"github.com/kanurag94/go-reverse-proxy/pkg/accesslog"
	"github.com/kanurag94/go-reverse-proxy/pkg/bump"
	"github.com/kanurag94/go-reverse-proxy/pkg/cache"
	"github.com/kanurag94/go-reverse-proxy/pkg/config"
	"github.com/kanurag94/go-reverse-proxy/pkg/intercept"
	"github.com/kanurag94/go-reverse-proxy/pkg/router"
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
	snap, err := router.Build(cfg, store, router.Options{})
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
	errs := make(chan error, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		h, err := live.Handler(l.Name)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: accesslog.New(h), ReadHeaderTimeout: 10 * time.Second}
		if len(l.TLS) > 0 {
			if srv.TLSConfig, err = tlsConfig(l.TLS); err != nil {
				return fmt.Errorf("listener %s: %w", l.Name, err)
			}
		}
		ln, err := net.Listen("tcp", l.Address)
		if err != nil {
			return err
		}
		if l.BumpDir() != "" {
			if ln, err = intercepting(ln, l, live); err != nil {
				return fmt.Errorf("listener %s: %w", l.Name, err)
			}
		}
		servers = append(servers, srv)
		slog.Info("listening", "listener", l.Name, "address", ln.Addr().String(), "tls", len(l.TLS) > 0, "bump", l.BumpDir() != "")
		go func() { errs <- fmt.Errorf("listener %s: %w", l.Name, serve(srv, ln)) }()
	}
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		return shutdown(servers, 30*time.Second)
	}
}

// serve serves ln, over TLS when srv has a TLS config. ServeTLS also turns on HTTP/2.
func serve(srv *http.Server, ln net.Listener) error {
	if srv.TLSConfig != nil {
		return srv.ServeTLS(ln, "", "")
	}
	return srv.Serve(ln)
}

// intercepting wraps ln so that l's configured names are bumped with l's CA and
// every other connection passes through to where it was going.
func intercepting(ln net.Listener, l config.Listener, live *router.Live) (net.Listener, error) {
	ca, err := bump.LoadOrCreate(l.BumpDir())
	if err != nil {
		return nil, fmt.Errorf("bump: %w", err)
	}
	slog.Info("bumping TLS; clients must trust the CA", "listener", l.Name, "ca", filepath.Join(l.BumpDir(), "ca.pem"))
	return intercept.NewListener(ln, ca, func(name string) bool { return live.HasDomain(l.Name, name) }), nil
}

// tlsConfig loads a listener's certificates. With several, crypto/tls picks the one
// matching the client's SNI, falling back to the first.
func tlsConfig(certs []config.Cert) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
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
	snap, err := router.Build(cfg, store, router.Options{})
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
