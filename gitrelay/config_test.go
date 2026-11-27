package main

import (
	"os"
	"testing"
)

func cleanConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LISTEN_ADDR", "MAX_CONCURRENT", "REQUESTS_PER_SECOND", "REQUEST_BURST", "GIT_REQUEST_MAX_BYTES",
	} {
		old, present := os.LookupEnv(name)
		os.Unsetenv(name)
		t.Cleanup(func() {
			if present {
				os.Setenv(name, old)
			} else {
				os.Unsetenv(name)
			}
		})
	}
}

func TestConfigurationValidation(t *testing.T) {
	cleanConfigEnv(t)
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" {
		t.Fatalf("default LISTEN_ADDR = %q, want 127.0.0.1:8080", c.Listen)
	}
	for _, listen := range []string{"[::1]:8080", "localhost:9090"} {
		t.Run("valid_"+listen, func(t *testing.T) {
			t.Setenv("LISTEN_ADDR", listen)
			if _, err := loadConfig(); err != nil {
				t.Fatalf("valid LISTEN_ADDR %q rejected: %v", listen, err)
			}
		})
	}
	for _, tt := range []struct{ name, value string }{
		{"LISTEN_ADDR", "bad"},
		{"LISTEN_ADDR", ":8080"},
		{"LISTEN_ADDR", "0.0.0.0:8080"},
		{"LISTEN_ADDR", "[::]:8080"},
		{"LISTEN_ADDR", "10.0.0.1:8080"},
		{"LISTEN_ADDR", "example.com:8080"},
		{"LISTEN_ADDR", "127.0.0.1:0"},
		{"LISTEN_ADDR", "127.0.0.1:70000"},
		{"MAX_CONCURRENT", "0"},
		{"REQUEST_BURST", "10001"},
		{"GIT_REQUEST_MAX_BYTES", "1073741825"},
		{"GIT_REQUEST_MAX_BYTES", "0"},
	} {
		t.Run(tt.name+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.name, tt.value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("invalid %s=%q accepted", tt.name, tt.value)
			}
		})
	}
}
