package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/accesslog"
	"github.com/kanurag94/go-reverse-proxy/internal/cache"
	"github.com/kanurag94/go-reverse-proxy/internal/config"
	"github.com/kanurag94/go-reverse-proxy/internal/router"
	"github.com/kanurag94/go-reverse-proxy/internal/watch"
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
	if err := supported(cfg); err != nil {
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
	errs := make(chan error, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		h, err := live.Handler(l.Name)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", l.Address)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: accesslog.New(h), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		slog.Info("listening", "listener", l.Name, "address", ln.Addr().String())
		go func() { errs <- fmt.Errorf("listener %s: %w", l.Name, srv.Serve(ln)) }()
	}
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		return shutdown(servers, 30*time.Second)
	}
}

// supported rejects what this binary cannot serve yet.
func supported(cfg *config.Config) error {
	for _, l := range cfg.Listeners {
		if len(l.TLS) > 0 {
			return fmt.Errorf("listener %s: TLS listeners are not supported yet", l.Name)
		}
	}
	return nil
}

// reload swaps in the config in content. On any error the current one keeps serving.
func reload(content []byte, store *cache.Store, live *router.Live) error {
	cfg, err := config.Parse(content)
	if err != nil {
		return err
	}
	if err := supported(cfg); err != nil {
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
