package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Only tests replace transport routing. The production resolver/dialer has no
// configuration switch or allow-private mode.
func localUpstream(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	upstream := httptest.NewTLSServer(handler)
	t.Cleanup(upstream.Close)
	local, _ := url.Parse(upstream.URL)
	tr := upstream.Client().Transport
	t.Cleanup(func() { upstream.Client().CloseIdleConnections() })
	return &http.Client{
		CheckRedirect: checkRedirect,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			u := *r.URL
			u.Host, u.Scheme = local.Host, local.Scheme
			clone.URL = &u
			clone.Host = r.URL.Host
			resp, err := tr.RoundTrip(clone)
			if resp != nil {
				resp.Request = r
			}
			return resp, err
		}),
	}
}

func testService(t *testing.T, c config, client *http.Client) *server {
	t.Helper()
	return newServer(c, testCache(t, c), client, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func proxyRequest(t *testing.T, s *server, method, path string, headers http.Header, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	for k, values := range headers {
		r.Header[k] = values
	}
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	return out
}

const releasePath = "/https://github.com/o/r/releases/download/v1/test.bin"

func TestProxyCacheRangeAndConditions(t *testing.T) {
	var calls atomic.Int64
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" ||
			r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Secret") != "" {
			t.Error("credentials or hop headers forwarded")
		}
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("compression not disabled")
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Connection", "X-Secret")
		w.Header().Set("X-Secret", "upstream-secret")
		http.ServeContent(w, r, "test.bin", time.Unix(1700000000, 0), strings.NewReader("0123456789"))
	})
	s := testService(t, testConfig(t), client)
	headers := http.Header{
		"Cookie": {"session=secret"}, "Proxy-Authorization": {"Basic secret"},
		"Connection": {"X-Secret"}, "X-Secret": {"request-secret"},
	}
	got := proxyRequest(t, s, "GET", releasePath, headers, "")
	if got.Code != 200 || got.Body.String() != "0123456789" || got.Header().Get("X-Gitrelay-Cache") != "MISS" {
		t.Fatal(got.Code, got.Body.String(), got.Header())
	}
	if got.Header().Get("X-Secret") != "" || got.Header().Get("Set-Cookie") != "" {
		t.Fatal("unsafe response headers forwarded")
	}
	for _, tt := range []struct {
		method string
		h      http.Header
		code   int
		body   string
	}{
		{"GET", nil, 200, "0123456789"},
		{"HEAD", nil, 200, ""},
		{"GET", http.Header{"Range": {"bytes=2-5"}}, 206, "2345"},
		{"GET", http.Header{"Range": {"bytes=100-"}}, 416, "invalid range: failed to overlap\n"},
		{"GET", http.Header{"If-None-Match": {`"v1"`}}, 304, ""},
		{"GET", http.Header{"If-Match": {`"other"`}}, 412, ""},
		{"GET", http.Header{"If-Modified-Since": {time.Unix(1700000000, 0).UTC().Format(http.TimeFormat)}}, 304, ""},
		{"GET", http.Header{"If-Unmodified-Since": {time.Unix(1600000000, 0).UTC().Format(http.TimeFormat)}}, 412, ""},
		{"GET", http.Header{"Range": {"bytes=2-5"}, "If-Range": {`"other"`}}, 200, "0123456789"},
	} {
		out := proxyRequest(t, s, tt.method, releasePath, tt.h, "")
		if out.Code != tt.code || out.Body.String() != tt.body {
			t.Errorf("%s %v: got %d %q", tt.method, tt.h, out.Code, out.Body.String())
		}
		if out.Header().Get("X-Gitrelay-Cache") != "HIT" {
			t.Error("not a hit", out.Header())
		}
	}
	if calls.Load() != 1 {
		t.Fatal("cache hit called upstream", calls.Load())
	}
}

func TestRangeMissAndNoStoreAreNeverCached(t *testing.T) {
	var calls atomic.Int64
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.Contains(r.URL.Path, "private") {
			w.Header().Set("Cache-Control", "private, no-store")
			w.Header().Set("Set-Cookie", "session=secret")
		}

		http.ServeContent(w, r, "test", time.Time{}, strings.NewReader("0123456789"))
	})
	s := testService(t, testConfig(t), client)
	out := proxyRequest(t, s, "GET", releasePath, http.Header{"Range": {"bytes=4-6"}}, "")
	if out.Code != 206 || out.Body.String() != "456" || out.Header().Get("Content-Range") != "bytes 4-6/10" {
		t.Fatal("range miss changed semantics", out.Code, out.Body.String())
	}
	for i := 0; i < 2; i++ {
		out = proxyRequest(t, s, "GET", releasePath+"private", nil, "")
		if out.Header().Get("X-Gitrelay-Cache") != "MISS" || out.Header().Get("Set-Cookie") != "" {
			t.Fatal("private response cached or leaked cookie")
		}
	}
	if used, count := s.cache.stats(); used != 0 || count != 0 || calls.Load() != 3 {
		t.Fatal("uncacheable requests occupied cache", used, count, calls.Load())
	}
}

func TestConditionalMissAndUnknownLength(t *testing.T) {
	var calls atomic.Int64
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.Header().Set("ETag", `"v1"`)
			w.WriteHeader(304)
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		io.WriteString(w, "chunked")
	})
	s := testService(t, testConfig(t), client)
	if out := proxyRequest(t, s, "GET", releasePath, http.Header{"If-None-Match": {`"v1"`}}, ""); out.Code != 304 {
		t.Fatal("conditional miss changed status", out.Code)
	}
	for i := 0; i < 2; i++ {
		out := proxyRequest(t, s, "GET", releasePath, nil, "")
		if out.Body.String() != "chunked" || out.Header().Get("X-Gitrelay-Cache") != "MISS" {
			t.Fatal("unknown-length stream changed", out.Body.String())
		}
	}
	if used, count := s.cache.stats(); used != 0 || count != 0 || calls.Load() != 3 {
		t.Fatal("conditional or unknown-length response cached")
	}
}

func TestFollowRedirectWithoutSecrets(t *testing.T) {
	var calls atomic.Int64
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Error("redirect leaked secrets")
		}
		if r.Host == "github.com" {
			w.Header().Set("Location", "https://release-assets.githubusercontent.com/github-production-release-asset/1/file?sig=secret")
			w.WriteHeader(302)
			return
		}
		w.Header().Set("Content-Length", "5")
		io.WriteString(w, "asset")
	})
	s := testService(t, testConfig(t), client)
	out := proxyRequest(t, s, "GET", releasePath, http.Header{"Cookie": {"secret"}}, "")
	if out.Code != 200 || out.Body.String() != "asset" || out.Header().Get("Location") != "" || calls.Load() != 2 {
		t.Fatal("release redirect failed", out.Code, out.Body.String(), calls.Load())
	}
}

func TestRejectRedirectAndRedactLogs(t *testing.T) {
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://127.0.0.1/secret?sig=TOPSECRET")
		w.WriteHeader(302)
	})
	s := testService(t, testConfig(t), client)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewJSONHandler(&logs, nil))
	out := proxyRequest(t, s, "GET", releasePath, nil, "")
	if out.Code != 502 || strings.Contains(logs.String(), "TOPSECRET") || strings.Contains(logs.String(), "127.0.0.1") {
		t.Fatal("unsafe redirect or leaked log", out.Code, logs.String())
	}
}

func TestAuthMethodsMetricsAndLimits(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected upstream call")
		return nil, nil
	})}
	s := testService(t, testConfig(t), client)
	for _, tt := range []struct {
		path string
		code int
	}{
		{releasePath, 401}, {"/metrics", 401}, {"/healthz", 200},
	} {
		out := httptest.NewRecorder()
		s.ServeHTTP(out, httptest.NewRequest("GET", tt.path, nil))
		if out.Code != tt.code {
			t.Fatal(tt.path, out.Code)
		}
	}
	out := proxyRequest(t, s, "GET", "/metrics", nil, "")
	if out.Code != 200 || !strings.Contains(out.Body.String(), "gitrelay_cache_reserved_bytes 0") {
		t.Fatal("metrics not available", out.Body.String())
	}
	if out := proxyRequest(t, s, "POST", releasePath, nil, "body"); out.Code != 400 {
		t.Fatal("unsupported method accepted")
	}
	s.slots = make(chan struct{}, 1)
	s.slots <- struct{}{}
	if out := proxyRequest(t, s, "GET", releasePath, nil, ""); out.Code != 429 {
		t.Fatal("concurrency not limited")
	}
	<-s.slots
	s.mu.Lock()
	s.tokens, s.last = 0, time.Now().Add(time.Hour)
	s.mu.Unlock()
	if out := proxyRequest(t, s, "GET", releasePath, nil, ""); out.Code != 429 {
		t.Fatal("rate not limited")
	}
}

func TestGitReadOnlyBodyLimitsAndNoCache(t *testing.T) {
	var calls atomic.Int64
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Method == "POST" && string(body) != "0000" {
			t.Error("git request body changed", string(body))
		}
		if r.Header.Get("Git-Protocol") != "version=2" {
			t.Error("git protocol header lost")
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Content-Length", "4")
		io.WriteString(w, "0000")
	})
	s := testService(t, testConfig(t), client)
	h := http.Header{"Content-Type": {"application/x-git-upload-pack-request"}, "Git-Protocol": {"version=2"}}
	for _, method := range []string{"GET", "POST", "POST"} {
		path, body := "/https://github.com/o/r.git/info/refs?service=git-upload-pack", ""
		if method == "POST" {
			path, body = "/https://github.com/o/r.git/git-upload-pack", "0000"
		}
		if out := proxyRequest(t, s, method, path, h, body); out.Code != 200 || out.Body.String() != "0000" {
			t.Fatal("git relay failed", out.Code, out.Body.String())
		}
	}
	if used, count := s.cache.stats(); used != 0 || count != 0 || calls.Load() != 3 {
		t.Fatal("git cached")
	}
	path := "/https://github.com/o/r.git/git-upload-pack"
	if out := proxyRequest(t, s, "POST", path, h, strings.Repeat("x", 1025)); out.Code != 413 {
		t.Fatal("git body limit not enforced", out.Code)
	}
	if out := proxyRequest(t, s, "POST", path, nil, "0000"); out.Code != 415 {
		t.Fatal("git content type not enforced", out.Code)
	}
	if out := proxyRequest(t, s, "POST", "/https://github.com/o/r.git/git-receive-pack", h, "0000"); out.Code != 400 {
		t.Fatal("push accepted")
	}
}

func TestInterruptedUpstreamDoesNotCommit(t *testing.T) {
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "short")
	})
	s := testService(t, testConfig(t), client)
	func() {
		defer func() {
			if err := recover(); err != http.ErrAbortHandler {
				t.Fatalf("short response did not abort: %v", err)
			}
		}()
		proxyRequest(t, s, "GET", releasePath, nil, "")
	}()
	if used, count := s.cache.stats(); used != 0 || count != 0 {
		t.Fatal("partial download cached", used, count)
	}
	files, _ := filepath.Glob(filepath.Join(s.cfg.CacheDir, ".tmp-*"))
	if len(files) != 0 {
		t.Fatal("partial file leaked")
	}
}

func TestCancellationReachesUpstream(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		io.WriteString(w, "start")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	s := testService(t, testConfig(t), client)
	proxy := httptest.NewServer(s)
	defer proxy.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", proxy.URL+releasePath, nil)
	req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not cancelled")
	}
	// Server.Close waits for the handler's cache cleanup.
	proxy.Close()
	if used, count := s.cache.stats(); used != 0 || count != 0 {
		t.Fatal("cancelled stream cached", used, count)
	}
}

func TestLargeStreamWithoutWholeObjectBuffer(t *testing.T) {
	const size = 8 << 20
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(size))
		chunk := make([]byte, 32<<10)
		for i := 0; i < size/len(chunk); i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	c := testConfig(t)
	c.ObjectBytes = size
	c.CacheBytes = allocated(size)
	s := testService(t, c, client)
	proxy := httptest.NewServer(s)
	defer proxy.Close()
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", proxy.URL+releasePath, nil)
		req.SetBasicAuth(c.Username, c.Password)
		resp, err := proxy.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil || n != size {
			t.Fatal("large stream truncated", n, err)
		}
		if i == 0 {
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, count := s.cache.stats(); count == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("cache atomic commit did not complete")
				}
				time.Sleep(time.Millisecond)
			}
		}
		if i == 1 && resp.Header.Get("X-Gitrelay-Cache") != "HIT" {
			t.Fatal("large object not cached")
		}
	}
}

func TestDiskErrorsAreObservable(t *testing.T) {
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		io.WriteString(w, "ok")
	})
	s := testService(t, testConfig(t), client)
	// Deterministic failure even when tests run as root: use a regular file as
	// the cache directory instead of depending on filesystem permissions.
	bad := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(bad, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	s.cache.dir = bad
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	out := proxyRequest(t, s, "GET", releasePath, nil, "")
	if out.Code != 200 || out.Body.String() != "ok" || s.stats.errors.Load() == 0 ||
		!strings.Contains(logs.String(), "cache reservation failed") {
		t.Fatal("disk failure hidden", out.Code, logs.String())
	}
}

func TestTruncatedCacheAbortsAndLogs(t *testing.T) {
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		io.WriteString(w, "0123456789")
	})
	s := testService(t, testConfig(t), client)
	proxyRequest(t, s, "GET", releasePath, nil, "")
	target, err := parseTarget(httptest.NewRequest("GET", releasePath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(s.cache.path(cacheKey(target.URL.String())), cacheHeaderSize+2); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if err := recover(); err != http.ErrAbortHandler || s.stats.errors.Load() == 0 {
				t.Fatalf("truncated cache was not observable: %v", err)
			}
		}()
		proxyRequest(t, s, "GET", releasePath, nil, "")
	}()
	out := proxyRequest(t, s, "GET", releasePath, nil, "")
	if out.Code != 200 || out.Body.String() != "0123456789" || out.Header().Get("X-Gitrelay-Cache") != "MISS" {
		t.Fatal("corrupt cached object was not replaced")
	}
}
