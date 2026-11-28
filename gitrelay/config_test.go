package main

import (
	"os"
	"testing"
)

func cleanConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LISTEN_ADDR", "MAX_CONCURRENT", "REQUESTS_PER_SECOND", "REQUEST_BURST", "GIT_REQUEST_MAX_BYTES",
		"ENABLE_AUTH_PASSTHROUGH",
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
	if c.AuthPassthrough {
		t.Fatal("default AuthPassthrough = true, want false")
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
		{"ENABLE_AUTH_PASSTHROUGH", ""},
		{"ENABLE_AUTH_PASSTHROUGH", "yes"},
		{"ENABLE_AUTH_PASSTHROUGH", "2"},
	} {
		t.Run(tt.name+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.name, tt.value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("invalid %s=%q accepted", tt.name, tt.value)
			}
		})
	}
	for _, value := range []string{"true", "false", "1", "0"} {
		t.Setenv("ENABLE_AUTH_PASSTHROUGH", value)
		got, err := loadConfig()
		if err != nil || got.AuthPassthrough != (value == "true" || value == "1") {
			t.Fatalf("ENABLE_AUTH_PASSTHROUGH=%q err=%v AuthPassthrough=%v", value, err, got.AuthPassthrough)
		}
	}
}
