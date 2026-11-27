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
		target, err := healthcheckURL(os.Getenv("LISTEN_ADDR"))
		if err != nil {
			os.Exit(1)
		}
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(target)
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

func healthcheckURL(listenAddr string) (string, error) {
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return "", errors.New("invalid LISTEN_ADDR")
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func run(log *slog.Logger) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	client := newUpstreamClient(c.Concurrency)
	defer client.CloseIdleConnections()
	server := &http.Server{
		Addr:              c.Listen,
		Handler:           newHandler(c, handlerOptions{Client: client, Logger: log}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	log.Info("service started", "listen", c.Listen)
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
