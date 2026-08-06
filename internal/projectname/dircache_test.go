package projectname

import (
	"os"
	"path/filepath"
	"testing"
)

// mkRepo creates <parent>/<name>/.git and returns the repo root.
func mkRepo(t *testing.T, parent, name string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// The cache must be invisible: one DirCache serving several distinct dirs has to
// answer exactly as the bare functions do. A cache keyed carelessly (say, on the
// project basename rather than the dir) would pass a single-dir test and fail
// this one.
func TestDirCacheShouldAnswerAsTheUncachedFunctionsAcrossDistinctDirs(t *testing.T) {
	parent := t.TempDir()
	alpha := mkRepo(t, parent, "alpha")
	beta := mkRepo(t, parent, "beta")
	deep := filepath.Join(alpha, "internal", "pkg")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := Config{}
	dirs := &DirCache{}
	// Interleaved and repeated, so a stale entry bleeding between dirs shows up.
	for _, dir := range []string{alpha, beta, deep, alpha, beta, deep} {
		if got, want := dirs.ResolveForDir(cfg, dir, "task"), ResolveForDir(cfg, dir, "task"); got != want {
			t.Errorf("ResolveForDir(%q) = %q, want %q", dir, got, want)
		}
		if got, want := dirs.CanonicalForDir(cfg, dir), CanonicalForDir(cfg, dir); got != want {
			t.Errorf("CanonicalForDir(%q) = %q, want %q", dir, got, want)
		}
		if got, want := dirs.TaskForDir(cfg, dir, "alpha-task"), TaskForDir(cfg, dir, "alpha-task"); got != want {
			t.Errorf("TaskForDir(%q) = %q, want %q", dir, got, want)
		}
	}
}

// A dir three levels below its root must resolve to that root, not to itself —
// the property the cached path shares with ProjectRoot and the one a naive
// dir-keyed cache still has to get right on a miss.
func TestDirCacheShouldResolveASubdirectoryToItsRepoRoot(t *testing.T) {
	parent := t.TempDir()
	root := mkRepo(t, parent, "alpha")
	deep := filepath.Join(root, "internal", "pkg", "sub")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	dirs := &DirCache{}
	if got, want := dirs.CanonicalForDir(Config{}, deep), dirs.CanonicalForDir(Config{}, root); got != want {
		t.Errorf("subdirectory resolved to %q, want its root's %q", got, want)
	}
}

// A nil *DirCache is the documented "no cache to hand" case and must behave like
// the bare functions rather than panicking.
func TestDirCacheShouldResolveUncachedWhenNil(t *testing.T) {
	root := mkRepo(t, t.TempDir(), "alpha")
	var dirs *DirCache
	if got, want := dirs.ResolveForDir(Config{}, root, "task"), ResolveForDir(Config{}, root, "task"); got != want {
		t.Errorf("nil DirCache = %q, want %q", got, want)
	}
}

// The cache is scoped to one render precisely so a root moving under a live
// session is picked up by the next one. This pins that a FRESH cache sees a
// `git init` the previous cache could not — the staleness budget the design
// chose, stated as a test so a later process-lifetime cache has to break it
// deliberately rather than silently.
func TestDirCacheShouldSeeAGitInitOnTheNextRender(t *testing.T) {
	parent := t.TempDir()
	outer := mkRepo(t, parent, "outer")
	inner := filepath.Join(outer, "vendored")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{}

	first := &DirCache{}
	before := first.CanonicalForDir(cfg, inner)
	if want := first.CanonicalForDir(cfg, outer); before != want {
		t.Fatalf("before git init, %q should resolve to the outer repo %q", before, want)
	}

	if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Same cache: still the old answer, which is what "scoped to a render" means.
	if got := first.CanonicalForDir(cfg, inner); got != before {
		t.Errorf("within one render the answer should be stable, got %q then %q", before, got)
	}
	// Next render: a new cache, and the new root is visible.
	if got := (&DirCache{}).CanonicalForDir(cfg, inner); got == before {
		t.Errorf("a fresh cache should see the new repo root, still got %q", got)
	}
}
