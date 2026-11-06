package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	Listen          string
	Username        string
	Password        string
	CacheDir        string
	CacheBytes      int64
	ObjectBytes     int64
	CacheTTL        time.Duration
	Concurrency     int
	RequestsPerSec  int
	Burst           int
	GitRequestBytes int64
}

func loadConfig() (config, error) {
	c := config{
		Listen: ":8080", CacheDir: "/var/cache/gitrelay",
		CacheBytes: 20 << 30, ObjectBytes: 4 << 30, CacheTTL: 24 * time.Hour,
		Concurrency: 8, RequestsPerSec: 10, Burst: 20, GitRequestBytes: 32 << 20,
	}
	for name, dest := range map[string]*string{
		"LISTEN_ADDR": &c.Listen, "AUTH_USERNAME": &c.Username,
		"AUTH_PASSWORD": &c.Password, "CACHE_DIR": &c.CacheDir,
	} {
		if value, ok := os.LookupEnv(name); ok {
			*dest = value
		}
	}
	if file := os.Getenv("AUTH_PASSWORD_FILE"); file != "" {
		if c.Password != "" {
			return c, errors.New("set only one of AUTH_PASSWORD and AUTH_PASSWORD_FILE")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return c, fmt.Errorf("read AUTH_PASSWORD_FILE: %w", err)
		}
		c.Password = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	}
	for name, dest := range map[string]*int64{
		"CACHE_MAX_BYTES": &c.CacheBytes, "CACHE_OBJECT_MAX_BYTES": &c.ObjectBytes,
		"GIT_REQUEST_MAX_BYTES": &c.GitRequestBytes,
	} {
		if value, ok := os.LookupEnv(name); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 {
				return c, fmt.Errorf("%s must be a positive integer in bytes", name)
			}
			*dest = n
		}
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
	if value, ok := os.LookupEnv("CACHE_TTL"); ok {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 || d > 30*24*time.Hour {
			return c, errors.New("CACHE_TTL must be a positive duration at most 720h")
		}
		c.CacheTTL = d
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return c, fmt.Errorf("invalid LISTEN_ADDR: %w", err)
	}
	if c.Username == "" || strings.ContainsAny(c.Username, ":\r\n") ||
		len(c.Password) < 16 || strings.ContainsAny(c.Password, "\r\n") {
		return c, errors.New("AUTH_USERNAME is required (no colon/newline); AUTH_PASSWORD must contain at least 16 bytes and no newline")
	}
	if c.CacheDir == "" {
		return c, errors.New("CACHE_DIR must not be empty")
	}
	if c.CacheBytes > 1<<50 || c.ObjectBytes > c.CacheBytes-cacheHeaderSize || allocated(c.ObjectBytes) > c.CacheBytes {
		return c, errors.New("CACHE_OBJECT_MAX_BYTES plus metadata rounded to 4 KiB must fit CACHE_MAX_BYTES; cache limit must be at most 1 PiB")
	}
	if c.GitRequestBytes > 1<<30 {
		return c, errors.New("GIT_REQUEST_MAX_BYTES must be at most 1 GiB")
	}
	return c, nil
}
