package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

var (
	globalIPv6        = netip.MustParsePrefix("2000::/3")
	forbiddenNetworks = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.31.196.0/24"), netip.MustParsePrefix("192.52.193.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("192.175.48.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("3fff::/20"),
		netip.MustParsePrefix("2620:4f:8000::/48"),
	}
)

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() ||
		(ip.Is6() && !globalIPv6.Contains(ip)) {
		return false
	}
	for _, block := range forbiddenNetworks {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

type lookupFunc func(context.Context, string, string) ([]netip.Addr, error)
type dialFunc func(context.Context, string, string) (net.Conn, error)

func guardedDial(lookup lookupFunc, dial dialFunc) dialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || host != allowedUpstreamHost {
			return nil, errors.New("invalid upstream dial address")
		}
		ips, err := lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("upstream DNS returned no addresses")
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errors.New("upstream DNS returned a non-public address")
			}
		}
		var last error
		for _, ip := range ips {
			// Dial only the validated numeric address, preventing DNS rebinding.
			conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, fmt.Errorf("connect upstream: %w", last)
	}
}

func newUpstreamClient(concurrency int) *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:             nil,
		DialContext:       guardedDial(net.DefaultResolver.LookupNetIP, dialer.DialContext),
		ForceAttemptHTTP2: true, DisableCompression: true,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second, IdleConnTimeout: 90 * time.Second,
		MaxIdleConns: concurrency * 2, MaxIdleConnsPerHost: concurrency,
		MaxConnsPerHost: concurrency, MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
