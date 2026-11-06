package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		Username: "tester", Password: "only-a-test-password",
		CacheDir: t.TempDir(), CacheBytes: 1 << 20, ObjectBytes: 1 << 19,
		CacheTTL: time.Hour, Concurrency: 8, RequestsPerSec: 10000,
		Burst: 10000, GitRequestBytes: 1024,
	}
}

func testCache(t *testing.T, c config) *diskCache {
	t.Helper()
	cache, err := newDiskCache(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	return cache
}

func putCache(t *testing.T, d *diskCache, name, body string) {
	t.Helper()
	w, err := d.begin(cacheKey(name), int64(len(body)), http.Header{})
	if err != nil || w == nil {
		t.Fatalf("begin: %v, %v", w, err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.finish(true); err != nil {
		t.Fatal(err)
	}
}

func TestCacheReservationEvictionAndReaders(t *testing.T) {
	c := testConfig(t)
	c.CacheBytes = allocated(100) * 2
	d := testCache(t, c)
	putCache(t, d, "first", "one")
	f, _, release, err := d.open(cacheKey("first"))
	if err != nil || f == nil {
		t.Fatal("missing cached object", err)
	}
	putCache(t, d, "second", "two")
	putCache(t, d, "third", "three")
	if _, _, _, err := d.open("missing"); err != nil {
		t.Fatal(err)
	}
	if d.entries[cacheKey("second")] != nil {
		t.Fatal("LRU did not evict unused entry")
	}
	if _, err := f.Seek(cacheHeaderSize, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	if string(data) != "one" {
		t.Fatal("active reader evicted")
	}
	release()
	w, err := d.begin(cacheKey("pending"), 100, http.Header{})
	if err != nil || w == nil {
		t.Fatal("reservation failed", err)
	}
	used, _ := d.stats()
	if used > c.CacheBytes {
		t.Fatal("temporary files exceeded bound")
	}
	if other, _ := d.begin(cacheKey("pending"), 100, http.Header{}); other != nil {
		t.Fatal("duplicate in-progress writer")
	}
	w.Write([]byte("short"))
	if err := w.finish(true); err != nil {
		t.Fatal(err)
	}
	if d.entries[cacheKey("pending")] != nil {
		t.Fatal("incomplete body committed")
	}
	files, _ := filepath.Glob(filepath.Join(c.CacheDir, ".tmp-*"))
	if len(files) != 0 {
		t.Fatal("temporary file leaked")
	}
}

func TestCacheRestoreTTLAndExclusiveLock(t *testing.T) {
	c := testConfig(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := newDiskCache(c, log)
	if err != nil {
		t.Fatal(err)
	}
	putCache(t, d, "persist", "persisted")
	if other, err := newDiskCache(c, log); err == nil {
		other.Close()
		t.Fatal("shared writable cache directory allowed")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.CacheDir, ".tmp-interrupted"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.CacheDir, "broken.cache"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	d = testCache(t, c)
	f, _, release, err := d.open(cacheKey("persist"))
	if err != nil || f == nil {
		t.Fatal("restore failed", err)
	}
	release()
	if _, err := os.Stat(filepath.Join(c.CacheDir, ".tmp-interrupted")); !os.IsNotExist(err) {
		t.Fatal("interrupted file not removed")
	}
	d.mu.Lock()
	d.entries[cacheKey("persist")].meta.Expires = time.Now().Add(-time.Second)
	d.mu.Unlock()
	if f, _, _, _ := d.open(cacheKey("persist")); f != nil {
		t.Fatal("expired entry served")
	}
	putCache(t, d, "persist", "replacement")
}

func TestConcurrentCacheReservations(t *testing.T) {
	c := testConfig(t)
	c.CacheBytes = allocated(128) * 3
	d := testCache(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := d.begin(cacheKey(string(rune('a'+i))), 128, http.Header{})
			if err != nil {
				t.Error(err)
			}
			if used, _ := d.stats(); used > c.CacheBytes {
				t.Error("reservation exceeded capacity")
			}
			if w != nil {
				w.Write(make([]byte, 128))
				if err := w.finish(true); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestCachePolicy(t *testing.T) {
	for _, h := range []http.Header{
		{"Cache-Control": {"no-store"}}, {"Cache-Control": {"private"}},
		{"Cache-Control": {"no-cache"}}, {"Cache-Control": {"max-age=0"}},
		{"Set-Cookie": {"session=secret"}}, {"Vary": {"Authorization"}},
		{"Vary": {"*"}}, {"Content-Encoding": {"gzip"}},
		{"Cache-Control": {"public, max-age=30"}, "Age": {"30"}},
		{"Cache-Control": {"public, max-age=30"}, "Date": {time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)}},
		{"Expires": {time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)}},
	} {
		if _, ok := cacheLifetime(h, time.Hour); ok {
			t.Errorf("unsafe headers cacheable: %v", h)
		}
	}
	h := http.Header{"Cache-Control": {"public, max-age=30"}, "Age": {"10"}, "Vary": {"Accept-Encoding"}}
	if ttl, ok := cacheLifetime(h, time.Hour); !ok || ttl != 20*time.Second {
		t.Fatal("wrong upstream freshness", ttl, ok)
	}
}
