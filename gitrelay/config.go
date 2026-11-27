package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

type config struct {
	Listen          string
	Concurrency     int
	RequestsPerSec  int
	Burst           int
	GitRequestBytes int64
}

func loadConfig() (config, error) {
	c := config{
		Listen:          "127.0.0.1:8080",
		Concurrency:     8,
		RequestsPerSec:  10,
		Burst:           20,
		GitRequestBytes: 32 << 20,
	}
	if value, ok := os.LookupEnv("LISTEN_ADDR"); ok {
		c.Listen = value
	}
	if value, ok := os.LookupEnv("GIT_REQUEST_MAX_BYTES"); ok {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n <= 0 {
			return c, errors.New("GIT_REQUEST_MAX_BYTES must be a positive integer in bytes")
		}
		c.GitRequestBytes = n
	}
	for name, dest := range map[string]*int{
		"MAX_CONCURRENT": &c.Concurrency, "REQUESTS_PER_SECOND": &c.RequestsPerSec,
		"REQUEST_BURST": &c.Burst,
	} {
		if value, ok := os.LookupEnv(name); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 || n > 10000 {
				return c, fmt.Errorf("%s must be an integer from 1 to 10000", name)
			}
			*dest = n
		}
	}
	host, portStr, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c, fmt.Errorf("invalid LISTEN_ADDR: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return c, errors.New("LISTEN_ADDR port must be an integer from 1 to 65535")
	}
	if !strings.EqualFold(host, "localhost") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Unmap().IsLoopback() {
			return c, errors.New("LISTEN_ADDR must bind to a loopback host (127.0.0.1, ::1, or localhost)")
		}
	}
	if c.GitRequestBytes > 1<<30 {
		return c, errors.New("GIT_REQUEST_MAX_BYTES must be at most 1 GiB")
	}
	return c, nil
}
