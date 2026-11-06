package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	globalIPv6   = netip.MustParsePrefix("2000::/3")
)

type target struct {
	URL *url.URL
	Git bool
}

func parseTarget(r *http.Request) (target, error) {
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, errors.New("invalid upstream URL")
	}
	return classifyTarget(r.Method, u)
}

func classifyTarget(method string, u *url.URL) (target, error) {
	var t target
	if validateURL(u) != nil {
		return t, errors.New("invalid upstream URL")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 3 || !ownerPattern.MatchString(parts[0]) ||
		!repoPattern.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
		return t, errors.New("invalid owner or repository")
	}
	for _, part := range parts {
		if part == "" {
			return t, errors.New("empty upstream path component")
		}
	}
	t.URL = u
	switch u.Host {
	case "github.com":
		if strings.HasSuffix(parts[1], ".git") && len(parts[1]) > 4 {
			if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" &&
				method == http.MethodGet && u.RawQuery == "service=git-upload-pack" {
				t.Git = true
				return t, nil
			}
			if len(parts) == 3 && parts[2] == "git-upload-pack" &&
				method == http.MethodPost && u.RawQuery == "" {
				t.Git = true
				return t, nil
			}
		}
		if u.RawQuery != "" || (method != http.MethodGet && method != http.MethodHead) {
			break
		}
		if len(parts) >= 6 && parts[2] == "releases" && parts[3] == "download" {
			return t, nil
		}
		if len(parts) >= 4 && parts[2] == "archive" &&
			(strings.HasSuffix(u.Path, ".zip") || strings.HasSuffix(u.Path, ".tar.gz")) {
			return t, nil
		}
	case "raw.githubusercontent.com":
		if len(parts) >= 4 && u.RawQuery == "" &&
			(method == http.MethodGet || method == http.MethodHead) {
			return t, nil
		}
	}
	return target{}, errors.New("unsupported GitHub path or HTTP method")
}

func allowedHost(host string) bool {
	switch host {
	case "github.com", "raw.githubusercontent.com", "codeload.github.com",
		"release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	}
	return false
}

// Redirect-only hosts are deliberately not accepted as public entry points.
func validateURL(u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil ||
		u.Fragment != "" || !allowedHost(u.Host) ||
		strings.ContainsAny(u.Path, "\\\x00\r\n") {
		return errors.New("upstream must be an allowed HTTPS URL without userinfo or port")
	}
	for _, p := range strings.Split(u.Path, "/") {
		if p == "." || p == ".." {
			return errors.New("path traversal is not supported")
		}
	}
	switch u.Host {
	case "github.com", "raw.githubusercontent.com":
		return nil
	case "codeload.github.com":
		p := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(p) >= 4 && ownerPattern.MatchString(p[0]) && repoPattern.MatchString(p[1]) &&
			(p[2] == "zip" || p[2] == "tar.gz" || p[2] == "legacy.zip" || p[2] == "legacy.tar.gz") {
			return nil
		}
	case "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		if strings.HasPrefix(u.Path, "/github-production-release-asset/") ||
			(u.Host == "objects.githubusercontent.com" && strings.HasPrefix(u.Path, "/github-production-release-asset-2e65be/")) {
			return nil
		}
	}
	return errors.New("upstream redirect path is not allowed")
}

var forbiddenNetworks = []netip.Prefix{
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
		if err != nil || port != "443" || !allowedHost(host) {
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

func upstreamClient(concurrency int) *http.Client {
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
	return &http.Client{Transport: tr, CheckRedirect: checkRedirect}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || len(via) >= 5 {
		return errors.New("too many upstream redirects")
	}
	if err := validateURL(req.URL); err != nil {
		return err
	}
	if req.URL.Host == "github.com" || req.URL.Host == "raw.githubusercontent.com" {
		t, err := classifyTarget(req.Method, req.URL)
		if err != nil || t.Git {
			return errors.New("upstream redirect does not point to an allowed download path")
		}
	}
	if len(via) > 0 && (via[0].Method == http.MethodPost ||
		strings.Contains(via[0].URL.Path, ".git/")) {
		return errors.New("git upstream redirects are not supported")
	}
	req.Header = forwardHeaders(via[0].Header)
	return nil
}

func forwardHeaders(src http.Header) http.Header {
	h := make(http.Header)
	for _, name := range []string{
		"Range", "If-Range", "If-Match", "If-None-Match",
		"If-Modified-Since", "If-Unmodified-Since", "Content-Type", "Git-Protocol",
		"Cache-Control", "Pragma",
	} {
		for _, v := range src.Values(name) {
			h.Add(name, v)
		}
	}
	stripHopHeaders(h, src)
	h.Set("Accept-Encoding", "identity")
	h.Set("User-Agent", "gitrelay/1")
	return h
}

func stripHopHeaders(dst, src http.Header) {
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			dst.Del(strings.TrimSpace(token))
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		dst.Del(name)
	}
}
