package main

import (
	"os"
	"path/filepath"
	"testing"
)

func cleanConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LISTEN_ADDR", "CACHE_DIR", "CACHE_MAX_BYTES", "CACHE_OBJECT_MAX_BYTES",
		"CACHE_TTL", "MAX_CONCURRENT", "REQUESTS_PER_SECOND", "REQUEST_BURST", "GIT_REQUEST_MAX_BYTES",
	} {
		// Unset (rather than set empty) because explicit empty config is invalid.
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
	t.Setenv("AUTH_USERNAME", "tester")
	t.Setenv("AUTH_PASSWORD", "long-enough-test-password")
	t.Setenv("AUTH_PASSWORD_FILE", "")
}

func TestConfigurationValidation(t *testing.T) {
	cleanConfigEnv(t)
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, value string }{
		{"AUTH_USERNAME", ""}, {"AUTH_USERNAME", "a:b"}, {"AUTH_PASSWORD", "short"},
		{"AUTH_PASSWORD", "long-enough-test-password\n"}, {"CACHE_TTL", "0s"},
		{"CACHE_TTL", "721h"}, {"CACHE_MAX_BYTES", "-1"}, {"MAX_CONCURRENT", "0"},
		{"REQUEST_BURST", "10001"}, {"CACHE_OBJECT_MAX_BYTES", "999999999999"},
		{"GIT_REQUEST_MAX_BYTES", "1073741825"}, {"LISTEN_ADDR", "bad"}, {"CACHE_DIR", ""},
	} {
		t.Run(tt.name+tt.value, func(t *testing.T) {
			t.Setenv(tt.name, tt.value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("invalid %s accepted", tt.name)
			}
		})
	}
}

func TestPasswordFile(t *testing.T) {
	cleanConfigEnv(t)
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte("a-long-test-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_PASSWORD_FILE", file)
	if _, err := loadConfig(); err == nil {
		t.Fatal("ambiguous password source accepted")
	}
	t.Setenv("AUTH_PASSWORD", "")
	c, err := loadConfig()
	if err != nil || c.Password != "a-long-test-password" {
		t.Fatal("password file failed", err)
	}
	t.Setenv("AUTH_PASSWORD_FILE", file+"missing")
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing password file ignored")
	}
}
