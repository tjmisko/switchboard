package tailcache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeFS answers Stat from a table the test edits, counting calls.
type fakeFS struct {
	mu    sync.Mutex
	ids   map[string]Identity
	stats int
}

func newFakeFS() *fakeFS { return &fakeFS{ids: make(map[string]Identity)} }

func (f *fakeFS) set(path string, id Identity) {
	f.mu.Lock()
	f.ids[path] = id
	f.mu.Unlock()
}

func (f *fakeFS) Stat(path string) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats++
	id, ok := f.ids[path]
	if !ok {
		return Identity{}, fs.ErrNotExist
	}
	return id, nil
}

// counter is an extractor that counts its runs and returns their number.
type counter struct{ reads int }

func (c *counter) read(string) (int, error) {
	c.reads++
	return c.reads, nil
}

func TestLoadShouldStatOnceAndNotReadWhenTheFileIsUnchanged(t *testing.T) {
	fsys := newFakeFS()
	fsys.set("/a", Identity{Dev: 1, Ino: 2, Size: 10, ModTime: 100})
	c := New(fsys, 0)
	var x counter
	for range 3 {
		if v, err := Load(c, "/a", "k", x.read); err != nil || v != 1 {
			t.Fatalf("Load = %d, %v; want the first read's value", v, err)
		}
	}
	if x.reads != 1 || fsys.stats != 3 {
		t.Fatalf("reads=%d stats=%d over three loads, want one read and one stat per load", x.reads, fsys.stats)
	}
	if got := c.Stats(); got != (Stats{Stats: 3, Reads: 1, Hits: 2}) {
		t.Fatalf("Stats = %+v", got)
	}
}

func TestLoadShouldReReadWhenAnyPartOfTheIdentityMoves(t *testing.T) {
	base := Identity{Dev: 1, Ino: 2, Size: 10, ModTime: 100}
	for _, tc := range []struct {
		name string
		next Identity
	}{
		{"should re-read when an append grows the file", Identity{Dev: 1, Ino: 2, Size: 20, ModTime: 100}},
		{"should re-read when the file is truncated", Identity{Dev: 1, Ino: 2, Size: 4, ModTime: 100}},
		{"should re-read when the mtime moves at the same size", Identity{Dev: 1, Ino: 2, Size: 10, ModTime: 200}},
		{"should re-read when the file is replaced with the same size and mtime", Identity{Dev: 1, Ino: 3, Size: 10, ModTime: 100}},
		{"should re-read when the path moves to another device", Identity{Dev: 9, Ino: 2, Size: 10, ModTime: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newFakeFS()
			fsys.set("/a", base)
			c := New(fsys, 0)
			var x counter
			_, _ = Load(c, "/a", "k", x.read)
			fsys.set("/a", tc.next)
			if v, _ := Load(c, "/a", "k", x.read); v != 2 || x.reads != 2 {
				t.Fatalf("value %d after %d reads, want a re-read", v, x.reads)
			}
			if v, _ := Load(c, "/a", "k", x.read); v != 2 || x.reads != 2 {
				t.Fatalf("value %d after %d reads, want the re-read cached at the new identity", v, x.reads)
			}
		})
	}
}

func TestInvalidateShouldForceAReReadWhenAHookAnnouncesAChangeTheIdentityCannotSee(t *testing.T) {
	fsys := newFakeFS()
	id := Identity{Dev: 1, Ino: 2, Size: 10, ModTime: 100}
	fsys.set("/root.jsonl", id)
	fsys.set("/root/subagents/agent-a.jsonl", id)
	fsys.set("/other.jsonl", id)
	c := New(fsys, 0)
	var root, child, other counter
	_, _ = Load(c, "/root.jsonl", "k", root.read)
	_, _ = Load(c, "/root/subagents/agent-a.jsonl", "k", child.read)
	_, _ = Load(c, "/other.jsonl", "k", other.read)

	c.Invalidate("/root.jsonl")
	c.InvalidatePrefix("/root/")
	_, _ = Load(c, "/root.jsonl", "k", root.read)
	_, _ = Load(c, "/root/subagents/agent-a.jsonl", "k", child.read)
	_, _ = Load(c, "/other.jsonl", "k", other.read)
	if root.reads != 2 || child.reads != 2 || other.reads != 1 {
		t.Fatalf("reads root=%d child=%d other=%d, want only the invalidated root's files re-read", root.reads, child.reads, other.reads)
	}
}

func TestLoadShouldNotStoreAReadWhenAnInvalidationRacedIt(t *testing.T) {
	fsys := newFakeFS()
	fsys.set("/a", Identity{Size: 10, ModTime: 100})
	c := New(fsys, 0)
	reads := 0
	racing := func(string) (int, error) {
		reads++
		if reads == 1 {
			c.Invalidate("/a") // a hook arrives while the first read is in flight
		}
		return reads, nil
	}
	_, _ = Load(c, "/a", "k", racing)
	if v, _ := Load(c, "/a", "k", racing); v != 2 {
		t.Fatalf("value %d, want a fresh read: the first one may predate the hook's change", v)
	}
}

func TestLoadShouldRetryWhenTheExtractorFailed(t *testing.T) {
	fsys := newFakeFS()
	fsys.set("/a", Identity{Size: 10, ModTime: 100})
	c := New(fsys, 0)
	reads := 0
	failing := func(string) (int, error) {
		reads++
		if reads == 1 {
			return 0, errors.New("incomplete tail")
		}
		return reads, nil
	}
	if _, err := Load(c, "/a", "k", failing); err == nil {
		t.Fatal("the extractor's error was lost")
	}
	if v, err := Load(c, "/a", "k", failing); err != nil || v != 2 {
		t.Fatalf("Load = %d, %v; want the error not cached", v, err)
	}
}

func TestLoadShouldReadUncachedWhenThePathCannotBeStatted(t *testing.T) {
	c := New(newFakeFS(), 0)
	var x counter
	_, _ = Load(c, "/missing", "k", x.read)
	_, _ = Load(c, "/missing", "k", x.read)
	if x.reads != 2 || c.Len() != 0 {
		t.Fatalf("reads=%d remembered=%d, want every load read through and nothing kept", x.reads, c.Len())
	}
}

func TestLoadShouldKeepEachKeysValueWhenOneFileServesSeveralExtractions(t *testing.T) {
	fsys := newFakeFS()
	fsys.set("/a", Identity{Size: 10, ModTime: 100})
	c := New(fsys, 0)
	var first, second counter
	_, _ = Load(c, "/a", "first", first.read)
	_, _ = Load(c, "/a", "second", second.read)
	_, _ = Load(c, "/a", "first", first.read)
	_, _ = Load(c, "/a", "second", second.read)
	if first.reads != 1 || second.reads != 1 {
		t.Fatalf("reads first=%d second=%d, want one per key", first.reads, second.reads)
	}
}

func TestLoadShouldEvictTheLeastRecentlyUsedFileWhenOverTheLimit(t *testing.T) {
	fsys := newFakeFS()
	for _, p := range []string{"/a", "/b", "/c"} {
		fsys.set(p, Identity{Size: 1, ModTime: 1})
	}
	c := New(fsys, 2)
	var a, b, cc counter
	_, _ = Load(c, "/a", "k", a.read)
	_, _ = Load(c, "/b", "k", b.read)
	_, _ = Load(c, "/a", "k", a.read) // /b is now the least recently used
	_, _ = Load(c, "/c", "k", cc.read)
	if c.Len() != 2 {
		t.Fatalf("remembered %d files, want the limit of 2", c.Len())
	}
	_, _ = Load(c, "/a", "k", a.read)
	_, _ = Load(c, "/b", "k", b.read)
	if a.reads != 1 || b.reads != 2 {
		t.Fatalf("reads a=%d b=%d, want only the least recently used file evicted", a.reads, b.reads)
	}
}

func TestOSStatShouldSeeANewInodeWhenAFileIsReplacedWithTheSameSizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.jsonl")
	mtime := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("tail.jsonl", "first\n")
	c := New(OS{}, 0)
	read := func(p string) (string, error) {
		b, err := os.ReadFile(p)
		return string(b), err
	}
	if v, _ := Load(c, path, "k", read); v != "first\n" {
		t.Fatalf("first load = %q", v)
	}
	if err := os.Rename(write("next.jsonl", "secnd\n"), path); err != nil {
		t.Fatal(err)
	}
	if v, _ := Load(c, path, "k", read); v != "secnd\n" {
		t.Fatalf("load after replacement = %q, want the replacement's content", v)
	}
}
