package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

var (
	version   = "dev"
	commit    = ""
	buildTime = "unknown"
)

type cliCommand uint8

const (
	cmdServe cliCommand = iota + 1
	cmdHealthcheck
	cmdVersion
)

type buildInfo struct {
	Version   string
	Commit    string
	BuildTime string
}

func (b buildInfo) String() string {
	return fmt.Sprintf("gitrelay %s (commit %s, built %s)", b.Version, b.Commit, b.BuildTime)
}

func parseCommand(args []string) (cliCommand, error) {
	switch len(args) {
	case 0:
		return cmdServe, nil
	case 1:
		switch args[0] {
		case "healthcheck":
			return cmdHealthcheck, nil
		case "--version", "version":
			return cmdVersion, nil
		}
	}
	return 0, errors.New("usage: gitrelay [healthcheck | --version | version]")
}

func resolveBuildInfo(ver, rev, built string, bi *debug.BuildInfo) buildInfo {
	ver = strings.TrimSpace(ver)
	if ver == "" {
		ver = "dev"
	}
	rev = strings.TrimSpace(rev)
	if rev == "" {
		rev = vcsCommit(bi)
	}
	built = strings.TrimSpace(built)
	if built == "" {
		built = "unknown"
	}
	return buildInfo{
		Version:   ver,
		Commit:    rev,
		BuildTime: built,
	}
}

func vcsCommit(bi *debug.BuildInfo) string {
	if bi == nil {
		return "unknown"
	}
	var rev string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = strings.TrimSpace(s.Value)
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if modified && !strings.HasSuffix(rev, "-dirty") {
		rev += "-dirty"
	}
	return rev
}

func readBuildInfo() *debug.BuildInfo {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	return bi
}

func runCLI(args []string, stdout, stderr io.Writer, info buildInfo, runHealthcheck func() error, runServer func() error) int {
	cmd, err := parseCommand(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	switch cmd {
	case cmdVersion:
		fmt.Fprintln(stdout, info.String())
		return 0
	case cmdHealthcheck:
		if err := runHealthcheck(); err != nil {
			return 1
		}
		return 0
	default:
		if err := runServer(); err != nil {
			return 1
		}
		return 0
	}
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	info := resolveBuildInfo(version, commit, buildTime, readBuildInfo())
	var args []string
	if len(os.Args) > 1 {
		args = os.Args[1:]
	}
	os.Exit(runCLI(
		args,
		os.Stdout,
		os.Stderr,
		info,
		func() error { return checkHealth(os.Getenv("LISTEN_ADDR")) },
		func() error {
			err := run(log, info)
			if err != nil {
				log.Error("service stopped", "error", err)
			}
			return err
		},
	))
}

func checkHealth(listenAddr string) error {
	target, err := healthcheckURL(listenAddr)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected healthcheck status %d", resp.StatusCode)
	}
	return nil
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

func run(log *slog.Logger, info buildInfo) error {
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
	log.Info("service started", "listen", c.Listen, "version", info.Version, "commit", info.Commit, "built", info.BuildTime)
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
