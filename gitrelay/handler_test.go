package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testConfig() config {
	return config{
		Concurrency:     8,
		RequestsPerSec:  10000,
		Burst:           10000,
		GitRequestBytes: 1024,
	}
}

func localUpstream(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	upstream := httptest.NewTLSServer(handler)
	t.Cleanup(upstream.Close)
	local, _ := url.Parse(upstream.URL)
	tr := upstream.Client().Transport.(*http.Transport).Clone()
	tr.DisableCompression = true
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("upstream redirects are not supported")
		},
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

func proxyRequest(t *testing.T, h http.Handler, method, path string, headers http.Header, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, values := range headers {
		r.Header[k] = values
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	return out
}

const (
	infoRefsPath          = "/https://github.com/o/r.git/info/refs?service=git-upload-pack"
	uploadPackPath        = "/https://github.com/o/r.git/git-upload-pack"
	sentinelToken         = "ghp_sentinel"
	sentinelEncoded       = "eC1hY2Nlc3MtdG9rZW46Z2hwX3NlbnRpbmVs"
	sentinelAuthorization = "Basic " + sentinelEncoded
)

func scriptUpstream(status int, respHdr http.Header, body string, saw func(*http.Request)) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if saw != nil {
				saw(r)
			}
			hdr := make(http.Header, len(respHdr))
			for key, values := range respHdr {
				hdr[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
			}
			return &http.Response{
				StatusCode: status, Header: hdr, Body: io.NopCloser(strings.NewReader(body)),
				ContentLength: int64(len(body)), Request: r,
			}, nil
		}),
	}
}

func TestProxyStripsCredentialsAndForwardsCompressionOpaquely(t *testing.T) {
	var gzBody bytes.Buffer
	gzw := gzip.NewWriter(&gzBody)
	io.WriteString(gzw, "compressed-refs")
	gzw.Close()
	wantRaw := gzBody.Bytes()

	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" ||
			r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Referer") != "" ||
			r.Header.Get("Range") != "" || r.Header.Get("X-Secret") != "" ||
			r.Header.Get("Content-Encoding") != "" {
			t.Error("disallowed request headers forwarded")
		}
		if r.Header.Get("Accept-Encoding") != "deflate, gzip" {
			t.Errorf("Accept-Encoding = %q, want deflate, gzip", r.Header.Get("Accept-Encoding"))
		}
		if r.URL.Path != "/o/r.git/info/refs" {
			t.Errorf("upstream path = %q, want canonical /o/r.git/info/refs", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Set-Cookie", "session=secret")
		w.Header().Set("Connection", "X-Secret")
		w.Header().Set("X-Secret", "upstream-secret")
		w.Write(wantRaw)
	})
	var logs bytes.Buffer
	h := newHandler(testConfig(), handlerOptions{
		Client: client,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	headers := http.Header{
		"Authorization": {"Bearer secret"}, "Cookie": {"session=secret"},
		"Proxy-Authorization": {"Basic secret"}, "Referer": {"https://internal.example"},
		"Range": {"bytes=0-3"}, "Connection": {"X-Secret"}, "X-Secret": {"request-secret"},
		"Accept-Encoding": {"deflate, gzip"}, "Content-Encoding": {"gzip"},
	}
	got := proxyRequest(t, h, "GET", "/https://github.com/o/r/info/refs?service=git-upload-pack", headers, "")
	if got.Code != 200 || !bytes.Equal(got.Body.Bytes(), wantRaw) {
		t.Fatalf("code=%d body=%x want=%x", got.Code, got.Body.Bytes(), wantRaw)
	}
	if got.Header().Get("Content-Encoding") != "gzip" || got.Header().Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
		t.Fatal("git response headers lost", got.Header())
	}
	if got.Header().Get("X-Secret") != "" || got.Header().Get("Set-Cookie") != "" {
		t.Fatal("unsafe response headers forwarded", got.Header())
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal("invalid json log", err, logs.String())
	}
	if entry["action"] != "info_refs" || entry["repo"] != "o/r" || entry["upstream_ms"] == nil {
		t.Fatalf("missing observability fields: %v", entry)
	}
}

func TestUnknownLengthStream(t *testing.T) {
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		io.WriteString(w, "chunked")
	})
	h := newHandler(testConfig(), handlerOptions{Client: client})
	out := proxyRequest(t, h, "GET", infoRefsPath, nil, "")
	if out.Code != 200 || out.Body.String() != "chunked" {
		t.Fatal("unknown-length stream changed", out.Code, out.Body.String())
	}
}

func TestRejectRedirectAndRedactLogs(t *testing.T) {
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/o/new.git/info/refs?sig=TOPSECRET")
		w.WriteHeader(302)
	})
	var logs bytes.Buffer
	h := newHandler(testConfig(), handlerOptions{
		Client: client,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	out := proxyRequest(t, h, "GET", infoRefsPath, nil, "")
	if out.Code != 502 || strings.Contains(logs.String(), "TOPSECRET") || strings.Contains(logs.String(), "new.git") {
		t.Fatal("redirect not rejected or log leaked", out.Code, logs.String())
	}
}

func TestEndpointsMethodsMetricsAndLimits(t *testing.T) {
	noUpstream := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected upstream call")
		return nil, nil
	})}
	now := time.Unix(1700000000, 0)
	h := newHandler(testConfig(), handlerOptions{
		Client: noUpstream,
		Now:    func() time.Time { return now },
	})
	for _, method := range []string{http.MethodConnect, http.MethodPut, http.MethodDelete, http.MethodHead} {
		out := proxyRequest(t, h, method, infoRefsPath, nil, "")
		if out.Code != http.StatusMethodNotAllowed || out.Header().Get("Allow") != "GET, POST" {
			t.Fatalf("%s returned %d Allow=%q", method, out.Code, out.Header().Get("Allow"))
		}
	}
	if out := proxyRequest(t, h, "GET", "/healthz", nil, ""); out.Code != 200 || out.Body.String() != "ok\n" {
		t.Fatal("healthz failed", out.Code, out.Body.String())
	}
	if out := proxyRequest(t, h, "GET", "/https://github.com/o/r/archive/refs/heads/main.zip", nil, ""); out.Code != 400 {
		t.Fatalf("disallowed path returned %d", out.Code)
	}
	now = now.Add(42 * time.Second)
	metrics := proxyRequest(t, h, "GET", "/metrics", nil, "")
	if metrics.Code != 200 ||
		!strings.Contains(metrics.Body.String(), "gitrelay_rejected_method_total 4\n") ||
		!strings.Contains(metrics.Body.String(), "gitrelay_uptime_seconds 42\n") ||
		!strings.Contains(metrics.Body.String(), "gitrelay_active_requests 1\n") {
		t.Fatal("unexpected metrics output", metrics.Code, metrics.Body.String())
	}

	entered, release := make(chan struct{}), make(chan struct{})
	blockingClient := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, "0000")
	})
	c := testConfig()
	c.Concurrency = 1
	concHandler := newHandler(c, handlerOptions{Client: blockingClient})
	var firstCode int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		firstCode = proxyRequest(t, concHandler, "GET", infoRefsPath, nil, "").Code
	}()
	<-entered
	if out := proxyRequest(t, concHandler, "GET", infoRefsPath, nil, ""); out.Code != 429 {
		t.Fatalf("concurrency limit not enforced: got %d", out.Code)
	}
	close(release)
	wg.Wait()
	if firstCode != 200 {
		t.Fatalf("first in-flight request failed: %d", firstCode)
	}

	rateNow := time.Unix(1700000000, 0)
	rateCfg := config{Concurrency: 8, RequestsPerSec: 1, Burst: 1, GitRequestBytes: 1024}
	rateHandler := newHandler(rateCfg, handlerOptions{
		Client: noUpstream,
		Now:    func() time.Time { return rateNow },
	})
	if out := proxyRequest(t, rateHandler, "GET", "/metrics", nil, ""); out.Code != 200 {
		t.Fatalf("initial burst request failed: %d", out.Code)
	}
	if out := proxyRequest(t, rateHandler, "GET", "/metrics", nil, ""); out.Code != 429 {
		t.Fatalf("rate limit not enforced: got %d", out.Code)
	}
	rateNow = rateNow.Add(time.Second)
	if out := proxyRequest(t, rateHandler, "GET", "/metrics", nil, ""); out.Code != 200 {
		t.Fatalf("refilled token rejected: %d", out.Code)
	}
}

func TestGitUploadPackGzipAndBodyLimits(t *testing.T) {
	var calls atomic.Int64
	var sawGzip atomic.Bool
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Method == "POST" && r.Header.Get("Content-Encoding") == "gzip" {
			sawGzip.Store(true)
			if string(body) != "raw-gzip-payload" {
				t.Errorf("gzip body modified: %q", string(body))
			}
		} else if r.Method == "POST" && string(body) != "0000" {
			t.Error("git request body changed", string(body))
		}
		if r.Header.Get("Git-Protocol") != "version=2" {
			t.Error("git protocol header lost")
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Content-Length", "4")
		io.WriteString(w, "0000")
	})
	h := newHandler(testConfig(), handlerOptions{Client: client})
	hdr := http.Header{"Content-Type": {"application/x-git-upload-pack-request"}, "Git-Protocol": {"version=2"}}
	if out := proxyRequest(t, h, "GET", infoRefsPath, hdr, ""); out.Code != 200 || out.Body.String() != "0000" {
		t.Fatal("info/refs failed", out.Code, out.Body.String())
	}
	if out := proxyRequest(t, h, "POST", uploadPackPath, hdr, "0000"); out.Code != 200 || out.Body.String() != "0000" {
		t.Fatal("identity POST failed", out.Code, out.Body.String())
	}
	gzHdr := hdr.Clone()
	gzHdr.Set("Content-Encoding", "gzip")
	if out := proxyRequest(t, h, "POST", "/https://github.com/o/r/git-upload-pack", gzHdr, "raw-gzip-payload"); out.Code != 200 {
		t.Fatalf("gzip POST rejected: %d %s", out.Code, out.Body.String())
	}
	if !sawGzip.Load() || calls.Load() != 3 {
		t.Fatalf("sawGzip=%v calls=%d", sawGzip.Load(), calls.Load())
	}
	brHdr := hdr.Clone()
	brHdr.Set("Content-Encoding", "br")
	if out := proxyRequest(t, h, "POST", uploadPackPath, brHdr, "0000"); out.Code != 415 {
		t.Fatal("unsupported content-encoding accepted", out.Code)
	}
	if out := proxyRequest(t, h, "POST", uploadPackPath, hdr, strings.Repeat("x", 1025)); out.Code != 413 {
		t.Fatal("git body limit not enforced", out.Code)
	}
	if out := proxyRequest(t, h, "POST", uploadPackPath, nil, "0000"); out.Code != 415 {
		t.Fatal("git content type not enforced", out.Code)
	}
	if out := proxyRequest(t, h, "POST", "/https://github.com/o/r.git/git-receive-pack", hdr, "0000"); out.Code != 400 {
		t.Fatal("push accepted")
	}
}

func TestInterruptedUpstreamAborts(t *testing.T) {
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "short")
	})
	h := newHandler(testConfig(), handlerOptions{Client: client})
	func() {
		defer func() {
			if err := recover(); err != http.ErrAbortHandler {
				t.Fatalf("short response did not abort: %v", err)
			}
		}()
		proxyRequest(t, h, "GET", infoRefsPath, nil, "")
	}()
	metrics := proxyRequest(t, h, "GET", "/metrics", nil, "")
	if !strings.Contains(metrics.Body.String(), "gitrelay_upstream_errors_total 1\n") {
		t.Fatalf("interrupted stream not counted as upstream error: %s", metrics.Body.String())
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
	h := newHandler(testConfig(), handlerOptions{Client: client})
	proxy := httptest.NewServer(h)
	defer proxy.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", proxy.URL+infoRefsPath, nil)
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
}

func TestLargeStreamWithoutWholeObjectBuffer(t *testing.T) {
	const size = 8 << 20
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Content-Length", fmt.Sprint(size))
		chunk := make([]byte, 32<<10)
		for i := 0; i < size/len(chunk); i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	h := newHandler(testConfig(), handlerOptions{Client: client})
	proxy := httptest.NewServer(h)
	defer proxy.Close()
	req, _ := http.NewRequest("POST", proxy.URL+uploadPackPath, strings.NewReader("0000"))
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil || n != size {
		t.Fatal("large stream truncated", n, err)
	}
}

func TestAuthPassthroughHeaders(t *testing.T) {
	challenges := []string{`Basic realm="GitHub"`, `Bearer realm="GitHub"`}
	upstreamHdr := http.Header{"WWW-Authenticate": challenges, "Set-Cookie": {"session=secret"}}
	clientHeaders := http.Header{
		"Authorization": {sentinelAuthorization}, "Cookie": {"session=secret"},
		"Proxy-Authorization": {"Basic secret"}, "Referer": {"https://internal.example"},
	}
	for _, tt := range []struct {
		name                                string
		allow, hop, wantAuth, wantChallenge bool
		status                              int
	}{
		{"disabled", false, false, false, false, http.StatusUnauthorized},
		{"challenge", true, false, true, true, http.StatusUnauthorized},
		{"ok", true, false, true, false, http.StatusOK},
		{"forbidden", true, false, true, false, http.StatusForbidden},
		{"server_error", true, false, true, false, http.StatusInternalServerError},
		{"connection_hop", true, true, false, false, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := scriptUpstream(tt.status, upstreamHdr, "body", func(r *http.Request) {
				gotAuth := r.Header.Values("Authorization")
				switch {
				case tt.wantAuth && (len(gotAuth) != 1 || gotAuth[0] != sentinelAuthorization):
					t.Errorf("Authorization = %#v", gotAuth)
				case !tt.wantAuth && len(gotAuth) != 0:
					t.Errorf("Authorization = %#v", gotAuth)
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Referer") != "" {
					t.Error("stripped request headers forwarded")
				}
			})
			cfg := testConfig()
			cfg.AuthPassthrough = tt.allow
			hdr := clientHeaders.Clone()
			if tt.hop {
				hdr.Set("Connection", "Authorization")
			}
			got := proxyRequest(t, newHandler(cfg, handlerOptions{Client: client}), "GET", infoRefsPath, hdr, "")
			if got.Code != tt.status {
				t.Fatalf("code=%d body=%s", got.Code, got.Body.String())
			}
			vals := got.Header().Values("WWW-Authenticate")
			if tt.wantChallenge {
				if len(vals) != 2 || vals[0] != challenges[0] || vals[1] != challenges[1] {
					t.Fatalf("WWW-Authenticate = %#v", vals)
				}
			} else if len(vals) != 0 {
				t.Fatalf("WWW-Authenticate = %#v", vals)
			}
			if got.Header().Get("Set-Cookie") != "" || got.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("Set-Cookie=%q Cache-Control=%q", got.Header().Get("Set-Cookie"), got.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestForwardHeadersRejectsUntrustedUpstream(t *testing.T) {
	src := http.Header{"Authorization": {sentinelAuthorization, "Bearer second"}}
	for _, upstream := range []*url.URL{
		nil,
		{Scheme: "http", Host: "github.com"},
		{Scheme: "https", Host: "evil.example"},
	} {
		got := forwardHeaders(src, actionInfoRefs, upstream, true)
		if got.Get("Authorization") != "" {
			t.Errorf("upstream=%v Authorization=%q", upstream, got.Get("Authorization"))
		}
	}
	trusted := &url.URL{Scheme: "https", Host: "github.com"}
	got := forwardHeaders(src, actionInfoRefs, trusted, true)
	if vals := got.Values("Authorization"); len(vals) != 2 || vals[0] != sentinelAuthorization || vals[1] != "Bearer second" {
		t.Fatalf("trusted Authorization=%#v", vals)
	}
	if denied := forwardHeaders(src, actionInfoRefs, trusted, false); denied.Get("Authorization") != "" {
		t.Fatalf("allowAuth false Authorization=%q", denied.Get("Authorization"))
	}
}

func TestSentinelCredentialAbsentFromLogsAndErrors(t *testing.T) {
	auth := http.Header{"Authorization": {sentinelAuthorization}}
	noUpstream := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected upstream call")
		return nil, errors.New("unexpected upstream call")
	})}
	sentinelErr := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial " + sentinelToken + " " + sentinelEncoded)
	})}
	for _, tt := range []struct {
		name, path, body, log string
		client                *http.Client
		want                  int
	}{
		{"400", "/https://github.com/o/r/archive/refs/heads/main.zip", "", "", noUpstream, http.StatusBadRequest},
		{"401", infoRefsPath, "", "", scriptUpstream(http.StatusUnauthorized, http.Header{"WWW-Authenticate": {`Basic realm="GitHub"`}}, "unauthorized", nil), http.StatusUnauthorized},
		{"500", infoRefsPath, "", `"msg":"upstream error response"`, scriptUpstream(http.StatusInternalServerError, nil, "upstream", nil), http.StatusInternalServerError},
		{"502_upstream_error", infoRefsPath, "upstream request failed\n", `"msg":"upstream request failed"`, sentinelErr, http.StatusBadGateway},
		{"502_redirect", infoRefsPath, "unsupported upstream redirect\n", "", scriptUpstream(http.StatusFound, http.Header{"Location": {"https://github.com/o/new.git/info/refs?service=git-upload-pack"}}, "redirected", nil), http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.AuthPassthrough = true
			var logs bytes.Buffer
			h := newHandler(cfg, handlerOptions{Client: tt.client, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			got := proxyRequest(t, h, http.MethodGet, tt.path, auth, "")
			if got.Code != tt.want || (tt.body != "" && got.Body.String() != tt.body) {
				t.Fatalf("code=%d body=%q", got.Code, got.Body.String())
			}
			text := logs.String()
			if !strings.Contains(text, `"msg":"request"`) || (tt.log != "" && !strings.Contains(text, tt.log)) {
				t.Fatalf("logs=%s", text)
			}
			blob := text + got.Body.String() + fmt.Sprint(got.Header())
			if strings.Contains(blob, sentinelToken) || strings.Contains(blob, sentinelEncoded) {
				t.Fatalf("credential leaked status=%d logs=%s body=%q headers=%v", got.Code, text, got.Body.String(), got.Header())
			}
		})
	}
}
