package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/accesslog"
	"github.com/kanurag94/go-reverse-proxy/internal/cache"
	"github.com/kanurag94/go-reverse-proxy/internal/config"
	"github.com/kanurag94/go-reverse-proxy/internal/router"
)

func main() {
	path := flag.String("config", "config.json", "path to the JSON config")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := run(*path); err != nil {
		slog.Error("proxy stopped", "err", err)
		os.Exit(1)
	}
}

func run(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	for _, l := range cfg.Listeners {
		if len(l.TLS) > 0 {
			return fmt.Errorf("listener %s: TLS listeners are not supported yet", l.Name)
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil               // never send upstream traffic through $HTTP_PROXY
	transport.DisableCompression = true // pass Accept-Encoding through as the client sent it
	transport.ResponseHeaderTimeout = 10 * time.Second

	store := cache.NewStore(cfg.CacheLimits()) // one store for the process, shared by every snapshot
	snap, err := router.Build(cfg, transport, store)
	if err != nil {
		return err
	}
	live := router.NewLive(snap)

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
		slog.Info("listening", "listener", l.Name, "address", ln.Addr().String())
		go func() { errs <- fmt.Errorf("listener %s: %w", l.Name, srv.Serve(ln)) }()
	}
	return <-errs
}
