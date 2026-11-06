package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	cacheHeaderSize = 16 << 10
	cacheMaxEntries = 4096
)

type cacheMeta struct {
	Key     string      `json:"key"`
	Size    int64       `json:"size"`
	Expires time.Time   `json:"expires"`
	Headers http.Header `json:"headers"`
}

type cacheEntry struct {
	meta cacheMeta
	used time.Time
	refs int
}

type diskCache struct {
	mu      sync.Mutex
	dir     string
	max     int64
	object  int64
	ttl     time.Duration
	used    int64
	entries map[string]*cacheEntry
	pending map[string]bool
	lock    *os.File
	log     *slog.Logger
}

func cacheKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func allocated(size int64) int64 {
	return (cacheHeaderSize + size + 4095) / 4096 * 4096
}

func newDiskCache(c config, log *slog.Logger) (*diskCache, error) {
	if err := os.MkdirAll(c.CacheDir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(c.CacheDir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("cache directory is already in use: %w", err)
	}
	d := &diskCache{
		dir: c.CacheDir, max: c.CacheBytes, object: c.ObjectBytes, ttl: c.CacheTTL,
		entries: make(map[string]*cacheEntry), pending: make(map[string]bool),
		lock: lock, log: log,
	}
	if err := d.restore(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *diskCache) Close() error {
	return d.lock.Close()
}

func (d *diskCache) path(key string) string {
	return filepath.Join(d.dir, key+".cache")
}

func (d *diskCache) restore() error {
	files, err := os.ReadDir(d.dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".cache") && !strings.HasPrefix(name, ".tmp-") {
			continue
		}
		path := filepath.Join(d.dir, name)
		valid := false
		var meta cacheMeta
		var info os.FileInfo
		if file.Type().IsRegular() && strings.HasSuffix(name, ".cache") {
			f, openErr := os.Open(path)
			if openErr != nil {
				return openErr
			}
			buf := make([]byte, cacheHeaderSize)
			_, readErr := io.ReadFull(f, buf)
			info, err = f.Stat()
			closeErr := f.Close()
			if readErr == nil && err == nil && closeErr == nil &&
				json.Unmarshal([]byte(strings.TrimRight(string(buf), "\x00")), &meta) == nil {
				key, decodeErr := hex.DecodeString(meta.Key)
				valid = decodeErr == nil && len(key) == sha256.Size &&
					name == meta.Key+".cache" && meta.Size >= 0 && meta.Size <= d.object &&
					info.Size() == cacheHeaderSize+meta.Size && time.Now().Before(meta.Expires)
			}
		}
		if !valid {
			d.log.Warn("removing incomplete, expired or invalid cache file", "file", name)
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove cache file %s: %w", name, err)
			}
			continue
		}
		d.entries[meta.Key] = &cacheEntry{meta: meta, used: info.ModTime()}
		d.used += allocated(meta.Size)
	}
	for d.used > d.max || len(d.entries) > cacheMaxEntries {
		evicted, err := d.evict()
		if err != nil {
			return err
		}
		if !evicted {
			return errors.New("cannot enforce cache limit")
		}
	}
	return nil
}

// Call under mu (or during initialization); active readers are never evicted,
// so unlinked-but-open files cannot silently escape disk accounting.
func (d *diskCache) evict() (bool, error) {
	var key string
	var oldest *cacheEntry
	for k, e := range d.entries {
		if e.refs == 0 && (oldest == nil || e.used.Before(oldest.used)) {
			key, oldest = k, e
		}
	}
	if oldest == nil {
		return false, nil
	}
	if err := os.Remove(d.path(key)); err != nil {
		return false, fmt.Errorf("evict cache object %s: %w", key, err)
	}
	delete(d.entries, key)
	d.used -= allocated(oldest.meta.Size)
	return true, nil
}

func (d *diskCache) open(key string) (*os.File, cacheMeta, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.entries[key]
	if e == nil || !time.Now().Before(e.meta.Expires) {
		return nil, cacheMeta{}, nil, nil
	}
	f, err := os.Open(d.path(key))
	if err != nil {
		return nil, cacheMeta{}, nil, err
	}
	e.refs++
	e.used = time.Now()
	return f, e.meta, func() {
		if err := f.Close(); err != nil {
			d.log.Error("cache reader close failed", "error", err)
		}
		d.mu.Lock()
		e.refs--
		d.mu.Unlock()
	}, nil
}

func (d *diskCache) invalidate(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.entries[key]; e != nil {
		e.meta.Expires = time.Time{}
	}
}

type cacheWrite struct {
	cache *diskCache
	file  *os.File
	meta  cacheMeta
	wrote int64
	done  bool
}

func (d *diskCache) begin(key string, size int64, headers http.Header) (*cacheWrite, error) {
	if size < 0 || size > d.object || allocated(size) > d.max {
		return nil, nil
	}
	meta := cacheMeta{Key: key, Size: size, Expires: time.Now().Add(d.ttl), Headers: headers.Clone()}
	if remaining, ok := cacheLifetime(headers, d.ttl); ok {
		meta.Expires = time.Now().Add(remaining)
	} else {
		return nil, nil
	}
	data, err := json.Marshal(meta)
	if err != nil || len(data) > cacheHeaderSize {
		return nil, errors.New("cache metadata exceeds fixed header size")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending[key] || d.entries[key] != nil {
		if e := d.entries[key]; e == nil || time.Now().Before(e.meta.Expires) || e.refs > 0 {
			return nil, nil
		}
		if err := os.Remove(d.path(key)); err != nil {
			return nil, err
		}
		d.used -= allocated(d.entries[key].meta.Size)
		delete(d.entries, key)
	}
	for d.used+allocated(size) > d.max || len(d.entries)+len(d.pending) >= cacheMaxEntries {
		evicted, err := d.evict()
		if err != nil {
			return nil, err
		}
		if !evicted {
			return nil, nil
		}
	}
	f, err := os.CreateTemp(d.dir, ".tmp-")
	if err != nil {
		return nil, err
	}
	d.pending[key] = true
	d.used += allocated(size)
	header := make([]byte, cacheHeaderSize)
	copy(header, data)
	if _, err := f.Write(header); err != nil {
		closeErr := f.Close()
		removeErr := os.Remove(f.Name())
		if removeErr == nil {
			delete(d.pending, key)
			d.used -= allocated(size)
		}
		return nil, errors.Join(err, closeErr, removeErr)
	}
	return &cacheWrite{cache: d, file: f, meta: meta}, nil
}

func (w *cacheWrite) Write(p []byte) (int, error) {
	if w.done || int64(len(p)) > w.meta.Size-w.wrote {
		return 0, errors.New("cache body exceeds reserved size")
	}
	n, err := w.file.Write(p)
	w.wrote += int64(n)
	return n, err
}

func (w *cacheWrite) finish(commit bool) error {
	if w.done {
		return nil
	}
	w.done = true
	var err error
	if commit && w.wrote == w.meta.Size {
		err = w.file.Sync()
	} else {
		commit = false
	}
	err = errors.Join(err, w.file.Close())
	d := w.cache
	d.mu.Lock()
	defer d.mu.Unlock()
	if commit && err == nil {
		err = os.Rename(w.file.Name(), d.path(w.meta.Key))
		if err == nil {
			d.entries[w.meta.Key] = &cacheEntry{meta: w.meta, used: time.Now()}
			delete(d.pending, w.meta.Key)
			return nil
		}
	}
	removeErr := os.Remove(w.file.Name())
	if removeErr != nil {
		// Retain the reservation if cleanup fails; do not pretend space is free.
		return errors.Join(err, fmt.Errorf("remove temporary cache file: %w", removeErr))
	}
	delete(d.pending, w.meta.Key)
	d.used -= allocated(w.meta.Size)
	return err
}

func (d *diskCache) stats() (int64, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.used, len(d.entries)
}
