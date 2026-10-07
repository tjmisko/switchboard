// Package tailcache keeps what was extracted from a file's tail against the
// file's identity (device, inode, size and modification time), so a poll over
// an unchanged file costs one stat and no read (#98).
//
// A value is reused only while all four agree. An append moves the size, a
// truncation shrinks it, and a replacement moves the inode even when size and
// mtime match. What the identity cannot see, a rewrite in place within the
// clock's mtime granularity, is what event invalidation covers: a hook for the
// root, a binding change and Forget drop the root's entries (Invalidate,
// InvalidatePrefix), and periodic reconciliation re-reads after any of them.
//
// No I/O runs under the cache's lock. Errors are never cached: a failed
// extraction is retried on the next load. Values are the extractors' small
// derived results, never raw file content, and the cache is bounded.
package tailcache

import (
	"os"
	"strings"
	"sync"
	"syscall"
)

// Identity is what decides whether a file changed since it was last read.
type Identity struct {
	Dev, Ino uint64
	Size     int64
	// ModTime is the modification time in Unix nanoseconds.
	ModTime int64
}

// FS is the stat seam: the operating system's in production, a fake in tests.
type FS interface {
	Stat(path string) (Identity, error)
}

// OS stats real files.
type OS struct{}

// Stat returns path's identity. Device and inode are zero where the platform
// does not report them, which leaves size and mtime to decide.
func (OS) Stat(path string) (Identity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		id.Dev, id.Ino = uint64(stat.Dev), uint64(stat.Ino)
	}
	return id, nil
}

// Stats counts the cache's work: Stats is identity checks, Reads is extractor
// runs, Hits is loads answered without one.
type Stats struct {
	Stats, Reads, Hits uint64
}

// DefaultLimit bounds the files the cache remembers. Each holds only small
// derived values, so the bound is about tracked transcripts, not bytes.
const DefaultLimit = 2048

// Cache maps a path to its identity and the values extracted at it, one per
// extraction key.
type Cache struct {
	fs    FS
	limit int

	mu    sync.Mutex
	files map[string]*file
	tick  uint64
	// epoch moves on every invalidation; a read that began before one does
	// not store its result, since it may predate the change it announced.
	epoch uint64
	stats Stats
}

type file struct {
	id     Identity
	values map[string]any
	used   uint64
}

// New returns a cache that stats through fs and remembers at most limit
// files (DefaultLimit when limit <= 0).
func New(fs FS, limit int) *Cache {
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &Cache{fs: fs, limit: limit, files: make(map[string]*file)}
}

var (
	defaultMu    sync.RWMutex
	defaultCache = New(OS{}, DefaultLimit)
)

// Default is the process-wide cache the providers' tail readers share.
func Default() *Cache {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultCache
}

// SetDefault replaces the process-wide cache and returns a function restoring
// the previous one. It exists for tests that count a reader's work.
func SetDefault(c *Cache) (restore func()) {
	defaultMu.Lock()
	previous := defaultCache
	defaultCache = c
	defaultMu.Unlock()
	return func() {
		defaultMu.Lock()
		defaultCache = previous
		defaultMu.Unlock()
	}
}

// Load returns read(path)'s value for key, reusing the value extracted at the
// file's current identity when there is one. A path that cannot be stat'ed is
// read uncached, so the extractor reports the failure in its own terms.
func Load[V any](c *Cache, path, key string, read func(string) (V, error)) (V, error) {
	id, err := c.fs.Stat(path)
	c.mu.Lock()
	c.stats.Stats++
	if err != nil {
		delete(c.files, path)
		c.stats.Reads++
		c.mu.Unlock()
		return read(path)
	}
	if f := c.files[path]; f != nil && f.id == id {
		if value, ok := f.values[key]; ok {
			c.tick++
			f.used = c.tick
			c.stats.Hits++
			c.mu.Unlock()
			return value.(V), nil
		}
	}
	epoch := c.epoch
	c.stats.Reads++
	c.mu.Unlock()

	value, err := read(path)
	if err != nil {
		return value, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return value, nil
	}
	f := c.files[path]
	if f == nil || f.id != id {
		f = &file{id: id, values: make(map[string]any, 1)}
		c.files[path] = f
	}
	f.values[key] = value
	c.tick++
	f.used = c.tick
	c.evictLocked()
	return value, nil
}

// evictLocked drops the least recently used files over the limit.
func (c *Cache) evictLocked() {
	for len(c.files) > c.limit {
		oldest, oldestUsed := "", uint64(0)
		for path, f := range c.files {
			if oldest == "" || f.used < oldestUsed {
				oldest, oldestUsed = path, f.used
			}
		}
		delete(c.files, oldest)
	}
}

// Invalidate drops every value extracted from the given paths.
func (c *Cache) Invalidate(paths ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	for _, path := range paths {
		delete(c.files, path)
	}
}

// InvalidatePrefix drops every value extracted from a path under prefix (a
// directory, given with its trailing separator). An empty prefix drops
// nothing.
func (c *Cache) InvalidatePrefix(prefix string) {
	if prefix == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	for path := range c.files {
		if strings.HasPrefix(path, prefix) {
			delete(c.files, path)
		}
	}
}

// Stats returns the work counted so far.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Len is the number of files the cache remembers.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.files)
}
