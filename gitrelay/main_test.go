package main

import "testing"

func TestHealthcheckURL(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		ok       bool
	}{
		{"", "http://127.0.0.1:8080/healthz", true},
		{"127.0.0.1:18080", "http://127.0.0.1:18080/healthz", true},
		{"[::1]:9090", "http://[::1]:9090/healthz", true},
		{"bad", "", false},
	} {
		got, err := healthcheckURL(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("healthcheckURL(%q) = (%q, %v), want (%q, ok=%v)", tt.in, got, err, tt.want, tt.ok)
		}
	}
}
