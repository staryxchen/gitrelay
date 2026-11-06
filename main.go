package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8080/healthz")
		if err != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	if err := run(log); err != nil {
		log.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	cache, err := newDiskCache(c, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := cache.Close(); err != nil {
			log.Error("cache lock close failed", "error", err)
		}
	}()
	client := upstreamClient(c.Concurrency)
	defer client.CloseIdleConnections()
	server := &http.Server{
		Addr: c.Listen, Handler: newServer(c, cache, client, log),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		MaxHeaderBytes: 32 << 10,
		// No whole-body or write deadline: large downloads and slow git fetches
		// remain streaming. Admission limits bound concurrent resource use.
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	log.Info("service started", "listen", c.Listen, "cache_limit_bytes", c.CacheBytes)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Warn("graceful shutdown timed out; interrupting active streams", "error", err)
			if closeErr := server.Close(); closeErr != nil {
				return closeErr
			}
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
