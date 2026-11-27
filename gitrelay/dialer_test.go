package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

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
	for _, addr := range []string{
		"example.com:443",
		"raw.githubusercontent.com:443",
		"codeload.github.com:443",
		"github.com:80",
	} {
		if _, err := dial(context.Background(), "tcp", addr); err == nil {
			t.Fatalf("disallowed dial address %s accepted", addr)
		}
	}
}
