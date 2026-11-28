package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var copyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32<<10)
		return &buf
	},
}

type handlerOptions struct {
	Client *http.Client
	Logger *slog.Logger
	Now    func() time.Time
}

type counters struct {
	requests            atomic.Int64
	infoRefs            atomic.Int64
	uploadPack          atomic.Int64
	reqBytes            atomic.Int64
	respBytes           atomic.Int64
	upstreamErrors      atomic.Int64
	clientAborts        atomic.Int64
	rejectedRateLimit   atomic.Int64
	rejectedConcurrency atomic.Int64
	rejectedMethod      atomic.Int64
}

type handler struct {
	cfg     config
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time
	started time.Time
	slots   chan struct{}
	stats   counters
	mu      sync.Mutex
	tokens  float64
	last    time.Time
}

func newHandler(cfg config, opts handlerOptions) *handler {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	started := nowFn()
	return &handler{
		cfg:     cfg,
		client:  opts.Client,
		log:     log,
		now:     nowFn,
		started: started,
		slots:   make(chan struct{}, cfg.Concurrency),
		tokens:  float64(cfg.Burst),
		last:    started,
	}
}

func (h *handler) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.tokens += now.Sub(h.last).Seconds() * float64(h.cfg.RequestsPerSec)
	if h.tokens > float64(h.cfg.Burst) {
		h.tokens = float64(h.cfg.Burst)
	}
	h.last = now
	if h.tokens < 1 {
		return false
	}
	h.tokens--
	return true
}

type responseLog struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseLog) WriteHeader(code int) {
	if w.status == 0 {
		w.Header().Set("Cache-Control", "private, no-store")
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *responseLog) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (h *handler) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	w := &responseLog{ResponseWriter: out}
	start := h.now()
	h.stats.requests.Add(1)
	var (
		action     string
		repo       string
		errorKind  string
		upstreamMS int64
		reqCounter *countingReader
	)
	defer func() {
		var reqBytes int64
		if reqCounter != nil {
			reqBytes = reqCounter.n
			h.stats.reqBytes.Add(reqBytes)
		}
		h.stats.respBytes.Add(w.bytes)
		attrs := []any{
			"method", r.Method,
			"status", w.status,
			"req_bytes", reqBytes,
			"resp_bytes", w.bytes,
			"duration_ms", h.now().Sub(start).Milliseconds(),
		}
		if action != "" {
			attrs = append(attrs, "action", action, "repo", repo, "upstream_ms", upstreamMS)
		}
		if errorKind != "" {
			attrs = append(attrs, "error_kind", errorKind)
		}
		h.log.Info("request", attrs...)
	}()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		h.stats.rejectedMethod.Add(1)
		errorKind = "method_not_allowed"
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
		return
	}
	if !h.allow() {
		h.stats.rejectedRateLimit.Add(1)
		errorKind = "rate_limited"
		w.Header().Set("Retry-After", "1")
		http.Error(w, "request rate exceeded", http.StatusTooManyRequests)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.stats.rejectedConcurrency.Add(1)
		errorKind = "concurrency_limited"
		w.Header().Set("Retry-After", "1")
		http.Error(w, "concurrent request limit exceeded", http.StatusTooManyRequests)
		return
	}
	if r.URL.Path == "/metrics" && r.Method == http.MethodGet {
		h.metrics(w)
		return
	}
	target, err := parseTarget(r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
	if err != nil {
		errorKind = "invalid_target"
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	repo = target.Owner + "/" + target.Repo
	if target.Action == actionInfoRefs {
		action = "info_refs"
		h.stats.infoRefs.Add(1)
	} else {
		action = "upload_pack"
		h.stats.uploadPack.Add(1)
	}
	if target.Action != actionUploadPack && (r.ContentLength > 0 || len(r.TransferEncoding) > 0) {
		errorKind = "unexpected_body"
		http.Error(w, "request body is not supported", http.StatusBadRequest)
		return
	}
	var body io.Reader
	if target.Action == actionUploadPack {
		ce := r.Header.Get("Content-Encoding")
		if r.Header.Get("Content-Type") != "application/x-git-upload-pack-request" ||
			(ce != "" && !strings.EqualFold(ce, "gzip")) {
			errorKind = "unsupported_media_type"
			http.Error(w, "expected git-upload-pack request (identity or gzip)", http.StatusUnsupportedMediaType)
			return
		}
		if r.ContentLength > h.cfg.GitRequestBytes {
			errorKind = "body_too_large"
			http.Error(w, "git request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		reqCounter = &countingReader{r: http.MaxBytesReader(w, r.Body, h.cfg.GitRequestBytes)}
		body = reqCounter
	}
	up, err := http.NewRequestWithContext(r.Context(), r.Method, target.UpstreamURL.String(), body)
	if err != nil {
		errorKind = "invalid_upstream_request"
		http.Error(w, "invalid upstream request", http.StatusBadRequest)
		return
	}
	up.Header = forwardHeaders(r.Header, target.Action, target.UpstreamURL, h.cfg.AuthPassthrough)
	if target.Action == actionUploadPack {
		up.ContentLength = r.ContentLength
	}
	upstreamStart := h.now()
	resp, err := h.client.Do(up)
	upstreamMS = h.now().Sub(upstreamStart).Milliseconds()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			errorKind = "body_too_large"
			http.Error(w, "git request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Context().Err() != nil {
			h.stats.clientAborts.Add(1)
			errorKind = "client_abort"
			return
		}
		h.stats.upstreamErrors.Add(1)
		errorKind = "upstream_error"
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			errorKind = "upstream_timeout"
		}
		h.log.Error("upstream request failed", "action", action, "repo", repo,
			"error_kind", errorKind, "error_type", fmt.Sprintf("%T", err))
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		h.stats.upstreamErrors.Add(1)
		errorKind = "upstream_5xx"
		h.log.Warn("upstream error response", "action", action, "repo", repo, "status", resp.StatusCode)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		next, redirErr := parseRedirectTarget(target, resp.StatusCode, resp.Header.Values("Location"))
		if redirErr != nil {
			h.stats.upstreamErrors.Add(1)
			errorKind = "upstream_redirect"
			if errors.Is(redirErr, errRedirectLoop) {
				errorKind = "upstream_redirect_loop"
			}
			http.Error(w, "unsupported upstream redirect", http.StatusBadGateway)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		w.Header().Set("Location", next.RelayPath())
		w.WriteHeader(resp.StatusCode)
		return
	}
	copyResponseHeaders(w.Header(), resp.Header, resp.StatusCode, h.cfg.AuthPassthrough)
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(resp.StatusCode)
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	writer := &flushWriter{dst: w, rc: http.NewResponseController(w)}
	n, copyErr := io.CopyBuffer(writer, resp.Body, *bufp)
	complete := copyErr == nil && r.Context().Err() == nil &&
		(resp.ContentLength < 0 || n == resp.ContentLength)
	if !complete {
		if r.Context().Err() != nil {
			h.stats.clientAborts.Add(1)
			errorKind = "client_abort"
		} else {
			h.stats.upstreamErrors.Add(1)
			errorKind = "stream_interrupted"
			h.log.Error("stream interrupted", "action", action, "repo", repo, "bytes", n)
		}
		panic(http.ErrAbortHandler)
	}
}

type flushWriter struct {
	dst io.Writer
	rc  *http.ResponseController
}

func (w *flushWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if err == nil {
		if flushErr := w.rc.Flush(); flushErr != nil && !errors.Is(flushErr, http.ErrNotSupported) {
			err = flushErr
		}
	}
	return n, err
}

func forwardHeaders(src http.Header, action gitAction, upstream *url.URL, allowAuth bool) http.Header {
	h := make(http.Header)
	copyHeader := func(name string) {
		for _, v := range src.Values(name) {
			h.Add(name, v)
		}
	}
	for _, name := range [...]string{"Content-Type", "Git-Protocol", "Accept", "Accept-Encoding", "Cache-Control", "Pragma"} {
		copyHeader(name)
	}
	if action == actionUploadPack {
		copyHeader("Content-Encoding")
	}
	if allowAuth && upstream != nil && upstream.Scheme == "https" && upstream.Host == allowedUpstreamHost {
		copyHeader("Authorization")
	}
	stripHopHeaders(h, src)
	h.Set("User-Agent", "gitrelay/1")
	return h
}

func copyResponseHeaders(dst, src http.Header, statusCode int, allowAuth bool) {
	copyHeader := func(name string) {
		key := http.CanonicalHeaderKey(name)
		if values, ok := src[key]; ok {
			dst[key] = append([]string(nil), values...)
		}
	}
	for _, name := range [...]string{"Content-Type", "Content-Length", "Content-Encoding"} {
		copyHeader(name)
	}
	if allowAuth && statusCode == http.StatusUnauthorized {
		copyHeader("WWW-Authenticate")
	}
	stripHopHeaders(dst, src)
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

func (h *handler) metrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	uptime := int64(h.now().Sub(h.started).Seconds())
	for _, m := range []struct {
		name string
		kind string
		n    int64
	}{
		{"requests_total", "counter", h.stats.requests.Load()},
		{"info_refs_total", "counter", h.stats.infoRefs.Load()},
		{"upload_pack_total", "counter", h.stats.uploadPack.Load()},
		{"active_requests", "gauge", int64(len(h.slots))},
		{"request_bytes_total", "counter", h.stats.reqBytes.Load()},
		{"response_bytes_total", "counter", h.stats.respBytes.Load()},
		{"upstream_errors_total", "counter", h.stats.upstreamErrors.Load()},
		{"client_aborts_total", "counter", h.stats.clientAborts.Load()},
		{"rejected_rate_limit_total", "counter", h.stats.rejectedRateLimit.Load()},
		{"rejected_concurrency_total", "counter", h.stats.rejectedConcurrency.Load()},
		{"rejected_method_total", "counter", h.stats.rejectedMethod.Load()},
		{"uptime_seconds", "gauge", uptime},
	} {
		fmt.Fprintf(w, "# TYPE gitrelay_%s %s\ngitrelay_%s %d\n", m.name, m.kind, m.name, m.n)
	}
}
