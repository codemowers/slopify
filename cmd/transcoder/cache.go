package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	size int64
	used time.Time
	pins int
}
type diskCache struct {
	root        *os.Root
	mu          sync.Mutex
	entries     map[string]*cacheEntry
	flights     map[string]chan struct{}
	used, limit int64
}

func openCache(dir string, limit int64) (*diskCache, error) {
	if limit <= 0 {
		return nil, errors.New("cache limit must be positive")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	c := &diskCache{root: root, entries: map[string]*cacheEntry{}, flights: map[string]chan struct{}{}, limit: limit}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		root.Close()
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() {
			continue
		}
		if strings.HasSuffix(name, ".part") {
			if err := root.Remove(name); err != nil {
				root.Close()
				return nil, err
			}
			continue
		}
		if !strings.HasSuffix(name, ".cache") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			root.Close()
			return nil, err
		}
		c.entries[name] = &cacheEntry{size: info.Size(), used: info.ModTime()}
		c.used += info.Size()
	}
	if !c.reserve(0) {
		root.Close()
		return nil, errors.New("cannot evict oversized cache")
	}
	return c, nil
}

// reserve includes in-progress files. Open readers pin entries against eviction.
func (c *diskCache) reserve(n int64) bool {
	if n > c.limit {
		return false
	}
	for c.used+n > c.limit {
		var oldest string
		var at time.Time
		for name, e := range c.entries {
			if e.pins == 0 && (oldest == "" || e.used.Before(at)) {
				oldest, at = name, e.used
			}
		}
		if oldest == "" {
			return false
		}
		e := c.entries[oldest]
		if err := c.root.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cache eviction", "err", err)
			return false
		}
		c.used -= e.size
		delete(c.entries, oldest)
	}
	c.used += n
	return true
}
func (c *diskCache) open(key string) (*os.File, func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return nil, nil, false
	}
	f, err := c.root.Open(key)
	if err != nil {
		return nil, nil, false
	}
	e.pins++
	e.used = time.Now()
	return f, func() { f.Close(); c.mu.Lock(); e.pins--; c.mu.Unlock() }, true
}
func (c *diskCache) begin(key string) (chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if done := c.flights[key]; done != nil {
		return done, false
	}
	done := make(chan struct{})
	c.flights[key] = done
	return done, true
}
func (c *diskCache) end(key string) {
	c.mu.Lock()
	done := c.flights[key]
	delete(c.flights, key)
	close(done)
	c.mu.Unlock()
}

type cacheWriter struct {
	cache     *diskCache
	key, part string
	file      *os.File
	size      int64
	closed    bool
}

func (c *diskCache) writer(key string) (*cacheWriter, error) {
	f, err := c.root.OpenFile(key+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return nil, err
	}
	return &cacheWriter{cache: c, key: key, part: key + ".part", file: f}, nil
}
func (w *cacheWriter) abortLocked() {
	if w.closed {
		return
	}
	w.closed = true
	w.file.Close()
	if err := w.cache.root.Remove(w.part); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Keep failed cleanup accounted for and eligible for a later eviction.
		w.cache.entries[w.part] = &cacheEntry{size: w.size, used: time.Now()}
		slog.Warn("cache partial cleanup", "err", err)
	} else {
		w.cache.used -= w.size
	}
}
func (w *cacheWriter) abort() { w.cache.mu.Lock(); defer w.cache.mu.Unlock(); w.abortLocked() }
func (w *cacheWriter) Write(p []byte) (int, error) {
	c := w.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	if !c.reserve(int64(len(p))) {
		w.abortLocked()
		return len(p), nil
	}
	n, err := w.file.Write(p)
	c.used -= int64(len(p) - n)
	w.size += int64(n)
	if err != nil {
		w.abortLocked()
		slog.Warn("cache write skipped", "err", err)
	}
	// Disk/cache failures never interrupt the live transcode response.
	return len(p), nil
}
func (w *cacheWriter) commit() error {
	c := w.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if w.closed {
		return nil
	}
	if err := w.file.Close(); err != nil {
		w.abortLocked()
		return err
	}
	if err := c.root.Rename(w.part, w.key); err != nil {
		w.abortLocked()
		return err
	}
	w.closed = true
	if old := c.entries[w.key]; old != nil {
		c.used -= old.size
	}
	c.entries[w.key] = &cacheEntry{size: w.size, used: time.Now()}
	return nil
}
