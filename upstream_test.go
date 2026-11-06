package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
)

func TestTargetBoundaries(t *testing.T) {
	tests := []struct {
		method, path string
		ok, git      bool
	}{
		{"GET", "/https://github.com/o/r/releases/download/v1/a.zip", true, false},
		{"HEAD", "/https://github.com/o/r/archive/refs/heads/main.tar.gz", true, false},
		{"GET", "/https://raw.githubusercontent.com/o/r/main/a%3Fb.txt", true, false},
		{"GET", "/https://github.com/o/r.git/info/refs?service=git-upload-pack", true, true},
		{"POST", "/https://github.com/o/r.git/git-upload-pack", true, true},
		{"GET", "/https://github.com/o/r.git/info/refs?service=git-receive-pack", false, false},
		{"POST", "/https://github.com/o/r.git/git-receive-pack", false, false},
		{"GET", "/https://github.com/o/r", false, false},
		{"GET", "/https://api.github.com/repos/o/r", false, false},
		{"GET", "/https://github.com.evil.test/o/r/archive/main.zip", false, false},
		{"GET", "/https://github.com:443/o/r/archive/main.zip", false, false},
		{"GET", "/https://u:p@github.com/o/r/archive/main.zip", false, false},
		{"GET", "/http://github.com/o/r/archive/main.zip", false, false},
		{"GET", "/https://127.0.0.1/o/r/archive/main.zip", false, false},
		{"GET", "/https://raw.githubusercontent.com/o/r/main/%2e%2e/a", false, false},
		{"GET", "/https://raw.githubusercontent.com/o/r/main/a%5cb", false, false},
		{"GET", "/https://avatars.githubusercontent.com/o/r/main/a", false, false},
		{"GET", "/https://codeload.github.com/o/r/zip/main", false, false},
		{"GET", "/https://github.com/o/r/archive/main.zip?token=secret", false, false},
		{"POST", "/https://github.com/o/r/releases/download/v1/a", false, false},
		{"DELETE", "/https://raw.githubusercontent.com/o/r/main/a", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			target, err := parseTarget(r)
			if (err == nil) != tt.ok || (err == nil && target.Git != tt.git) {
				t.Fatalf("target=%+v err=%v", target, err)
			}
			if err == nil && tt.path == "/https://raw.githubusercontent.com/o/r/main/a%3Fb.txt" &&
				(target.URL.RawQuery != "" || target.URL.Path != "/o/r/main/a?b.txt") {
				t.Fatal("escaped filename changed URL semantics")
			}
		})
	}
}

func TestRedirectValidation(t *testing.T) {
	original, _ := http.NewRequest("GET", "https://github.com/o/r/archive/main.zip", nil)
	original.Header.Set("Authorization", "secret")
	original.Header.Set("Cookie", "secret")
	for _, raw := range []string{
		"https://codeload.github.com/o/r/zip/refs/heads/main",
		"https://release-assets.githubusercontent.com/github-production-release-asset/1/asset?sig=secret",
		"https://objects.githubusercontent.com/github-production-release-asset/1/asset",
		"https://objects.githubusercontent.com/github-production-release-asset-2e65be/1/asset",
	} {
		req, _ := http.NewRequest("GET", raw, nil)
		req.Header.Set("Referer", "secret")
		if err := checkRedirect(req, []*http.Request{original}); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("Referer") != "" {
			t.Fatal("redirect leaked credentials")
		}
	}
	for _, raw := range []string{
		"http://github.com/o/r/archive/main.zip", "https://127.0.0.1/a",
		"https://evil.githubusercontent.com/a", "https://release-assets.githubusercontent.com/other/a",
		"https://codeload.github.com/o/r/other/main",
		"https://github.com/login", "https://raw.githubusercontent.com/o/r",
	} {
		req, _ := http.NewRequest("GET", raw, nil)
		if err := checkRedirect(req, []*http.Request{original}); err == nil {
			t.Fatalf("allowed %s", raw)
		}
	}
	git, _ := http.NewRequest("POST", "https://github.com/o/r.git/git-upload-pack", nil)
	redirect, _ := http.NewRequest("GET", "https://github.com/o/r", nil)
	if checkRedirect(redirect, []*http.Request{git}) == nil {
		t.Fatal("git redirect accepted")
	}
	if checkRedirect(redirect, []*http.Request{original, original, original, original, original}) == nil {
		t.Fatal("redirect loop accepted")
	}
}

func TestPublicAddresses(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.100.100.200", "0.0.0.0", "224.1.1.1",
		"192.0.2.1", "198.18.0.1", "203.0.113.1", "240.0.0.1",
		"fc00::1", "fe80::1", "ff02::1", "2001:db8::1", "2002:7f00:1::",
		"64:ff9b::7f00:1", "::ffff:127.0.0.1", "2001::1", "3fff::1",
	} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("allowed special address %s", raw)
		}
	}
	for _, raw := range []string{"140.82.112.3", "185.199.108.133", "2606:50c0:8000::154"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("rejected public address %s", raw)
		}
	}
}

func TestGuardedDialPinsDNS(t *testing.T) {
	called := 0
	dial := guardedDial(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("140.82.112.3")}, nil
	}, func(_ context.Context, _, addr string) (net.Conn, error) {
		called++
		if addr != "140.82.112.3:443" {
			t.Fatalf("dialed unresolved host: %s", addr)
		}
		return nil, errors.New("test connection")
	})
	dial(context.Background(), "tcp", "github.com:443")
	if called != 1 {
		t.Fatal("dial not attempted")
	}
	for _, ips := range [][]netip.Addr{
		{}, {netip.MustParseAddr("127.0.0.1")},
		{netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr("10.0.0.1")},
	} {
		dial = guardedDial(func(context.Context, string, string) ([]netip.Addr, error) { return ips, nil },
			func(context.Context, string, string) (net.Conn, error) { t.Fatal("unsafe dial"); return nil, nil })
		if _, err := dial(context.Background(), "tcp", "github.com:443"); err == nil {
			t.Fatal("unsafe DNS accepted")
		}
	}
	if _, err := dial(context.Background(), "tcp", "example.com:443"); err == nil {
		t.Fatal("unknown host accepted")
	}
	u, _ := url.Parse("https://github.com/o/r")
	if validateURL(u) != nil {
		t.Fatal("valid URL")
	}
}
