package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type counters struct {
	requests atomic.Int64
	hits     atomic.Int64
	misses   atomic.Int64
	errors   atomic.Int64
	rejected atomic.Int64
	bytes    atomic.Int64
}

type server struct {
	cfg    config
	client *http.Client
	cache  *diskCache
	log    *slog.Logger
	slots  chan struct{}
	stats  counters
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newServer(c config, cache *diskCache, client *http.Client, log *slog.Logger) *server {
	return &server{
		cfg: c, cache: cache, client: client, log: log, slots: make(chan struct{}, c.Concurrency),
		tokens: float64(c.Burst), last: time.Now(),
	}
}

func (s *server) allow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.tokens += now.Sub(s.last).Seconds() * float64(s.cfg.RequestsPerSec)
	if s.tokens > float64(s.cfg.Burst) {
		s.tokens = float64(s.cfg.Burst)
	}
	s.last = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

type responseLog struct {
	http.ResponseWriter
	status int
	bytes  int64
	err    error
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
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *server) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	w := &responseLog{ResponseWriter: out}
	start := time.Now()
	s.stats.requests.Add(1)
	defer func() {
		s.stats.bytes.Add(w.bytes)
		// Never log the URL, headers, raw query, credentials or signed redirects.
		s.log.Info("request", "method", r.Method, "status", w.status, "bytes", w.bytes,
			"duration_ms", time.Since(start).Milliseconds())
	}()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "private, no-store")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
		return
	}
	if !s.allow() {
		s.stats.rejected.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "request rate exceeded", http.StatusTooManyRequests)
		return
	}
	user, pass, ok := r.BasicAuth()
	userHash, passHash := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
	wantUser, wantPass := sha256.Sum256([]byte(s.cfg.Username)), sha256.Sum256([]byte(s.cfg.Password))
	if !ok || (subtle.ConstantTimeCompare(userHash[:], wantUser[:])&
		subtle.ConstantTimeCompare(passHash[:], wantPass[:])) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="gitrelay", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		s.stats.rejected.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "concurrent request limit exceeded", http.StatusTooManyRequests)
		return
	}
	if r.URL.Path == "/metrics" && r.Method == http.MethodGet {
		s.metrics(w)
		return
	}
	t, err := parseTarget(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodPost && (r.ContentLength > 0 || len(r.TransferEncoding) > 0) {
		http.Error(w, "request body is not supported", http.StatusBadRequest)
		return
	}
	if t.Git && r.Method == http.MethodPost {
		if r.Header.Get("Content-Type") != "application/x-git-upload-pack-request" ||
			r.Header.Get("Content-Encoding") != "" {
			http.Error(w, "expected unencoded git-upload-pack request", http.StatusUnsupportedMediaType)
			return
		}
		if r.ContentLength > s.cfg.GitRequestBytes {
			http.Error(w, "git request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.GitRequestBytes)
	}
	key := cacheKey(t.URL.String())
	bypass := requestBypassesCache(r)
	if !t.Git && !bypass {
		f, meta, release, err := s.cache.open(key)
		if err != nil {
			s.cacheError("cache read failed", key, err)
		} else if f != nil {
			defer release()
			s.stats.hits.Add(1)
			copyResponseHeaders(w.Header(), meta.Headers)
			w.Header().Set("X-Gitrelay-Cache", "HIT")
			w.Header().Set("Cache-Control", "private, no-store")
			modified, _ := http.ParseTime(meta.Headers.Get("Last-Modified"))
			http.ServeContent(w, r, "", modified, io.NewSectionReader(f, cacheHeaderSize, meta.Size))
			length, lengthErr := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
			short := (w.status == 200 || w.status == 206) && lengthErr == nil && w.bytes != length
			if r.Method != http.MethodHead && (short || w.err != nil || r.Context().Err() != nil) {
				if short && w.err == nil && r.Context().Err() == nil {
					s.cache.invalidate(key)
				}
				s.cacheError("cached response stream interrupted", key, errors.New("cache read or client write did not complete"))
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
	s.stats.misses.Add(1)
	var body io.Reader
	if r.Method == http.MethodPost {
		body = r.Body
	}
	up, err := http.NewRequestWithContext(r.Context(), r.Method, t.URL.String(), body)
	if err != nil {
		http.Error(w, "invalid upstream request", http.StatusBadRequest)
		return
	}
	up.Header = forwardHeaders(r.Header)
	if r.Method == http.MethodPost {
		up.ContentLength = r.ContentLength
	}
	resp, err := s.client.Do(up)
	if err != nil {
		s.stats.errors.Add(1)
		// net/http errors can contain the signed redirect URL: log type only.
		s.log.Error("upstream request failed", "key", key, "error_type", fmt.Sprintf("%T", err))
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "git request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		s.stats.errors.Add(1)
		s.log.Warn("upstream error response", "key", key, "status", resp.StatusCode)
	}
	// Redirects must be followed and validated by the client, never leaked to
	// the downloader (where credentials or signed URLs could escape).
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
		http.Error(w, "unsupported upstream redirect", http.StatusBadGateway)
		return
	}
	headers := make(http.Header)
	copyResponseHeaders(headers, resp.Header)
	var cw *cacheWrite
	if !t.Git && !bypass && cacheable(r, resp) {
		cw, err = s.cache.begin(key, resp.ContentLength, headers)
		if err != nil {
			s.cacheError("cache reservation failed", key, err)
		}
	}
	copyResponseHeaders(w.Header(), headers)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Gitrelay-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	writer := &streamWriter{dst: w, cache: cw, server: s, key: key}
	n, copyErr := io.CopyBuffer(writer, resp.Body, make([]byte, 32<<10))
	complete := copyErr == nil && r.Context().Err() == nil &&
		(resp.ContentLength < 0 || n == resp.ContentLength)
	if writer.cache != nil {
		if err := writer.cache.finish(complete); err != nil {
			s.cacheError("cache commit or cleanup failed", key, err)
		}
	}
	if !complete {
		s.stats.errors.Add(1)
		s.log.Error("stream interrupted", "key", key, "bytes", n)
		// Abort the HTTP stream; a short chunked response must not look complete.
		panic(http.ErrAbortHandler)
	}
}

type streamWriter struct {
	dst    io.Writer
	cache  *cacheWrite
	server *server
	key    string
}

func (w *streamWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if w.cache != nil {
		if _, cacheErr := w.cache.Write(p[:n]); cacheErr != nil {
			w.server.cacheError("cache body write failed", w.key, cacheErr)
			if finishErr := w.cache.finish(false); finishErr != nil {
				w.server.cacheError("cache cleanup failed", w.key, finishErr)
			}
			w.cache = nil
		}
	}
	if err == nil {
		if dst, ok := w.dst.(http.ResponseWriter); ok {
			// Flush small Git protocol packets as well as large download chunks.
			if flushErr := http.NewResponseController(dst).Flush(); flushErr != nil &&
				!errors.Is(flushErr, http.ErrNotSupported) {
				err = flushErr
			}
		}
	}
	return n, err
}

func (s *server) cacheError(message, key string, err error) {
	s.stats.errors.Add(1)
	s.log.Error(message, "key", key, "error", err)
}

func requestBypassesCache(r *http.Request) bool {
	cc := strings.ToLower(strings.Join(r.Header.Values("Cache-Control"), ","))
	return strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store") ||
		strings.Contains(cc, "max-age=0") || r.Header.Get("Pragma") != ""
}

func cacheLifetime(h http.Header, ttl time.Duration) (time.Duration, bool) {
	// Avoid caching anything requiring revalidation, private variants, or
	// responses with cookies. Unknown extension directives are conservative.
	if len(h.Values("Set-Cookie")) > 0 || h.Get("Content-Encoding") != "" {
		return 0, false
	}
	for _, vary := range strings.Split(strings.Join(h.Values("Vary"), ","), ",") {
		if vary = strings.TrimSpace(vary); vary != "" && !strings.EqualFold(vary, "Accept-Encoding") {
			return 0, false
		}
	}
	for _, directive := range strings.Split(strings.Join(h.Values("Cache-Control"), ","), ",") {
		pair := strings.SplitN(strings.TrimSpace(strings.ToLower(directive)), "=", 2)
		switch pair[0] {
		case "", "public", "immutable", "must-revalidate", "proxy-revalidate", "no-transform":
		case "max-age", "s-maxage":
			if len(pair) != 2 {
				return 0, false
			}
			seconds, err := strconv.ParseInt(strings.Trim(pair[1], `"`), 10, 64)
			if err != nil || seconds <= 0 {
				return 0, false
			}
			if seconds < int64(ttl/time.Second)+1 {
				ttl = time.Duration(seconds) * time.Second
			}
		default:
			return 0, false
		}
	}
	ageSeconds := int64(0)
	if date, err := http.ParseTime(h.Get("Date")); err == nil && time.Now().After(date) {
		ageSeconds = int64(time.Since(date) / time.Second)
	}
	if age := h.Get("Age"); age != "" {
		seconds, err := strconv.ParseInt(age, 10, 64)
		if err != nil || seconds < 0 {
			return 0, false
		}
		if seconds > ageSeconds {
			ageSeconds = seconds
		}
	}
	if ageSeconds > int64(ttl/time.Second) {
		return 0, false
	}
	ttl -= time.Duration(ageSeconds) * time.Second
	if expires, err := http.ParseTime(h.Get("Expires")); err == nil {
		if remaining := time.Until(expires); remaining < ttl {
			ttl = remaining
		}
	}
	return ttl, ttl > 0
}

func cacheable(r *http.Request, resp *http.Response) bool {
	if r.Method != http.MethodGet || resp.StatusCode != http.StatusOK ||
		resp.ContentLength < 0 || resp.Header.Get("Content-Range") != "" {
		return false
	}
	for _, h := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	_, ok := cacheLifetime(resp.Header, 24*time.Hour)
	return ok
}

func copyResponseHeaders(dst, src http.Header) {
	for _, name := range []string{
		"Content-Type", "Content-Length", "Content-Range", "Content-Disposition",
		"Content-Encoding", "ETag", "Last-Modified", "Accept-Ranges",
		"Cache-Control", "Expires", "Age", "Vary", "Date",
	} {
		if values, ok := src[http.CanonicalHeaderKey(name)]; ok {
			dst[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	stripHopHeaders(dst, src)
}

func (s *server) metrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	used, count := s.cache.stats()
	for _, m := range []struct {
		name string
		kind string
		n    int64
	}{
		{"requests_total", "counter", s.stats.requests.Load()},
		{"cache_hits_total", "counter", s.stats.hits.Load()},
		{"cache_misses_total", "counter", s.stats.misses.Load()},
		{"errors_total", "counter", s.stats.errors.Load()},
		{"rejected_total", "counter", s.stats.rejected.Load()},
		{"response_bytes_total", "counter", s.stats.bytes.Load()},
		{"cache_reserved_bytes", "gauge", used},
		{"cache_entries", "gauge", int64(count)},
		{"active_requests", "gauge", int64(len(s.slots))},
	} {
		fmt.Fprintf(w, "# TYPE gitrelay_%s %s\ngitrelay_%s %d\n", m.name, m.kind, m.name, m.n)
	}
}
