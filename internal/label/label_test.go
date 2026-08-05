package label

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
)

// sessionHome points HOME at a fresh temp dir for the duration of the test and
// returns the ~/.claude/sessions directory inside it, so a test can write (and
// rewrite) session files there the way Claude Code does.
func sessionHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeSessionName drops <dir>/<pid>.json carrying name, and returns its path.
func writeSessionName(t *testing.T, dir string, pid int, name string) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	body := fmt.Sprintf(`{"pid":%d,"name":%q}`, pid, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeSessionFile drops a ~/.claude/sessions/<pid>.json with the given name
// under a temp HOME, and points HOME at it for the duration of the test.
func writeSessionFile(t *testing.T, pid int, name string) {
	t.Helper()
	writeSessionName(t, sessionHome(t), pid, name)
}

func TestRawName_prefersClaudeSessionName(t *testing.T) {
	writeSessionFile(t, 4242, "from-claude")
	s := state.Session{
		PID:     4242,
		CWD:     "/home/u/Projects/Arachne",
		Wezterm: &state.WeztermInfo{WindowTitle: "✳ from-window"},
	}
	if got := RawName(s); got != "from-claude" {
		t.Errorf("RawName = %q, want from-claude", got)
	}
}

func TestRawName_fallsBackToWindowTitleStrippingSpinner(t *testing.T) {
	// HOME points at an empty temp dir, so there is no sessions file.
	t.Setenv("HOME", t.TempDir())
	s := state.Session{
		PID:     4243,
		CWD:     "/home/u/Projects/Arachne",
		Wezterm: &state.WeztermInfo{WindowTitle: "✳ assess-npm"},
	}
	if got := RawName(s); got != "assess-npm" {
		t.Errorf("RawName = %q, want assess-npm", got)
	}
}

func TestRawName_fallsBackToCwdBasename(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := state.Session{PID: 4244, CWD: "/home/u/Projects/Arachne"}
	if got := RawName(s); got != "Arachne" {
		t.Errorf("RawName = %q, want Arachne", got)
	}
}

// backdate rewrites path with body while restoring its original mtime, so the
// file's (mtime, size) stamp is unchanged even though its contents are not. That
// is the one edit a stamp-keyed cache is entitled to miss, and it is how a test
// observes from outside whether an answer was served from the cache.
func backdate(t *testing.T, path, body string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("backdated write changed the size (%d -> %d); the stamp must not move for this test to mean anything", before.Size(), after.Size())
	}
}

// bump rewrites path with body and pushes its mtime a second forward, so the
// stamp moves regardless of the filesystem's timestamp granularity. Callers pass
// a same-length body so mtime alone is doing the invalidating.
func bump(t *testing.T, path, body string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	next := before.ModTime().Add(time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatal(err)
	}
}

func TestNameCache_RawName_agreesWithTheUncachedLookupAtEveryFallback(t *testing.T) {
	dir := sessionHome(t)
	writeSessionName(t, dir, 5100, "named-on-disk")

	cases := []struct {
		what string
		sess state.Session
		want string
	}{
		{
			what: "claude session name",
			sess: state.Session{PID: 5100, CWD: "/home/u/Projects/Arachne", Wezterm: &state.WeztermInfo{WindowTitle: "✳ from-window"}},
			want: "named-on-disk",
		},
		{
			what: "window title",
			sess: state.Session{PID: 5101, CWD: "/home/u/Projects/Arachne", Wezterm: &state.WeztermInfo{WindowTitle: "✳ from-window"}},
			want: "from-window",
		},
		{
			what: "cwd basename",
			sess: state.Session{PID: 5102, CWD: "/home/u/Projects/Arachne"},
			want: "Arachne",
		},
		{
			what: "pid",
			sess: state.Session{PID: 5103},
			want: "pid 5103",
		},
	}

	c := &NameCache{}
	for _, tc := range cases {
		want := RawName(tc.sess)
		if want != tc.want {
			t.Fatalf("%s: uncached RawName = %q, want %q", tc.what, want, tc.want)
		}
		// Twice: the first call fills the entry, the second serves it.
		for i := range 2 {
			if got := c.RawName(tc.sess); got != want {
				t.Errorf("%s: call %d cached RawName = %q, want %q", tc.what, i+1, got, want)
			}
		}
	}
}

func TestNameCache_RawName_reportsTheNewNameWhenTheSessionIsRenamed(t *testing.T) {
	dir := sessionHome(t)
	path := writeSessionName(t, dir, 5200, "aaa")
	s := state.Session{PID: 5200, CWD: "/home/u/Projects/Arachne"}

	c := &NameCache{}
	if got := c.RawName(s); got != "aaa" {
		t.Fatalf("RawName = %q, want aaa", got)
	}
	// A `/name bbb` rewrite. The new name is the same length as the old, so the
	// size is unchanged and mtime alone has to carry the invalidation.
	bump(t, path, `{"pid":5200,"name":"bbb"}`)
	if got := c.RawName(s); got != "bbb" {
		t.Errorf("after rename RawName = %q, want bbb", got)
	}
}

func TestNameCache_RawName_servesTheCachedNameWhenTheStampHasNotMoved(t *testing.T) {
	dir := sessionHome(t)
	path := writeSessionName(t, dir, 5300, "aaa")
	s := state.Session{PID: 5300, CWD: "/home/u/Projects/Arachne"}

	c := &NameCache{}
	if got := c.RawName(s); got != "aaa" {
		t.Fatalf("RawName = %q, want aaa", got)
	}
	backdate(t, path, `{"pid":5300,"name":"bbb"}`)
	if got := c.RawName(s); got != "aaa" {
		t.Errorf("RawName = %q after a stamp-preserving rewrite, want the cached aaa (the file was re-read)", got)
	}
	// The uncached path is unaffected — it never memoizes anything.
	if got := RawName(s); got != "bbb" {
		t.Errorf("uncached RawName = %q, want bbb", got)
	}
}

func TestNameCache_RawName_readsTheFileOnceItAppears(t *testing.T) {
	dir := sessionHome(t)
	s := state.Session{PID: 5400, CWD: "/home/u/Projects/Arachne"}

	c := &NameCache{}
	// No file yet: the "" answer is cached against the zero stamp.
	if got := c.RawName(s); got != "Arachne" {
		t.Fatalf("RawName = %q with no session file, want the cwd basename Arachne", got)
	}
	writeSessionName(t, dir, 5400, "now-named")
	if got := c.RawName(s); got != "now-named" {
		t.Errorf("RawName = %q after the session file appeared, want now-named", got)
	}
}

func TestNameCache_RawName_followsHomeWhenItMovesUnderTheProcess(t *testing.T) {
	first := sessionHome(t)
	firstPath := writeSessionName(t, first, 5900, "aaa")
	s := state.Session{PID: 5900, CWD: "/home/u/Projects/Arachne"}

	c := &NameCache{}
	if got := c.RawName(s); got != "aaa" {
		t.Fatalf("RawName = %q, want aaa", got)
	}

	// A second home holding a DIFFERENT name for the same pid, stamped
	// identically to the first so only the path distinguishes them. Without the
	// path in the key the stale name would be served.
	second := sessionHome(t)
	secondPath := writeSessionName(t, second, 5900, "bbb")
	stamp, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(secondPath, stamp.ModTime(), stamp.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := c.RawName(s); got != "bbb" {
		t.Errorf("RawName = %q after HOME moved, want bbb", got)
	}
}

func TestNameCache_RawName_keepsPidsIndependent(t *testing.T) {
	dir := sessionHome(t)
	writeSessionName(t, dir, 5500, "first")
	writeSessionName(t, dir, 5501, "second")
	one := state.Session{PID: 5500, CWD: "/home/u/Projects/Arachne"}
	two := state.Session{PID: 5501, CWD: "/home/u/Projects/Arachne"}

	c := &NameCache{}
	for i := range 3 {
		if got := c.RawName(one); got != "first" {
			t.Fatalf("pass %d: pid 5500 RawName = %q, want first", i, got)
		}
		if got := c.RawName(two); got != "second" {
			t.Fatalf("pass %d: pid 5501 RawName = %q, want second", i, got)
		}
	}
}

func TestNameCache_RawName_readsTheDiskWhenTheCacheIsNil(t *testing.T) {
	dir := sessionHome(t)
	path := writeSessionName(t, dir, 5600, "aaa")
	s := state.Session{PID: 5600, CWD: "/home/u/Projects/Arachne"}

	var c *NameCache
	if got := c.RawName(s); got != "aaa" {
		t.Fatalf("nil cache RawName = %q, want aaa", got)
	}
	// A nil cache memoizes nothing, so even a stamp-preserving rewrite shows.
	backdate(t, path, `{"pid":5600,"name":"bbb"}`)
	if got := c.RawName(s); got != "bbb" {
		t.Errorf("nil cache RawName = %q, want bbb", got)
	}
}

func TestNameCache_Chip_matchesTheUncachedChip(t *testing.T) {
	dir := sessionHome(t)
	writeSessionName(t, dir, 5700, "assess-npm")
	s := state.Session{PID: 5700, CWD: "/home/u/Projects/Arachne"}
	cfg := projectname.Load()

	want := Chip(cfg, s)
	c := &NameCache{}
	for i := range 2 {
		if got := c.Chip(cfg, s); got != want {
			t.Errorf("call %d: cached Chip = %q, want %q", i+1, got, want)
		}
	}
}

func TestNameCache_RawName_boundsWhatItRetainsAcrossManyPids(t *testing.T) {
	dir := sessionHome(t)
	c := &NameCache{}
	// Every pid here is a miss (no file), which is the shape of the leak: a
	// long-lived bar naming sessions that have since exited.
	for pid := 9000; pid < 9000+3*maxNameCacheEntries; pid++ {
		c.RawName(state.Session{PID: pid, CWD: "/home/u/Projects/Arachne"})
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > maxNameCacheEntries {
		t.Errorf("cache retained %d entries, want at most %d", n, maxNameCacheEntries)
	}
	// Overflow must not cost correctness: a live session still names correctly.
	writeSessionName(t, dir, 9999, "still-right")
	if got := c.RawName(state.Session{PID: 9999, CWD: "/home/u/Projects/Arachne"}); got != "still-right" {
		t.Errorf("RawName = %q after the cache overflowed, want still-right", got)
	}
}

// benchSessionCount is this machine's usual live session count — the number of
// sessions renderSlot names on every emission, since each slot must fit the
// whole label set to agree with its neighbours.
const benchSessionCount = 13

// benchSessions writes a session file per pid under a temp HOME and returns the
// sessions naming them, the input shape of a single bar emission.
func benchSessions(b *testing.B) []state.Session {
	b.Helper()
	home := b.TempDir()
	b.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	sessions := make([]state.Session, benchSessionCount)
	for i := range sessions {
		pid := 7000 + i
		body := fmt.Sprintf(`{"pid":%d,"sessionId":"b5c7fd65-5733-4ce2-a0fa-932b91d2c02%d","cwd":"/home/u/Projects/Arachne","startedAt":1785950796170,"kind":"interactive","name":"assess-npm-vulnerabilities","nameSource":"derived","status":"busy","updatedAt":1785957054072}`, pid, i%10)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
		sessions[i] = state.Session{PID: pid, CWD: "/home/u/Projects/Arachne"}
	}
	return sessions
}

// BenchmarkRawName is one emission's worth of naming through the uncached path.
func BenchmarkRawName(b *testing.B) {
	sessions := benchSessions(b)
	b.ResetTimer()
	for range b.N {
		for i := range sessions {
			if RawName(sessions[i]) == "" {
				b.Fatal("empty name")
			}
		}
	}
}

// BenchmarkNameCache_RawName is the same emission through the cache, with the
// session files sitting still — the overwhelmingly common case, since they move
// about once per six session-minutes against a ~1s render cadence.
func BenchmarkNameCache_RawName(b *testing.B) {
	sessions := benchSessions(b)
	c := &NameCache{}
	for i := range sessions {
		c.RawName(sessions[i])
	}
	b.ResetTimer()
	for range b.N {
		for i := range sessions {
			if c.RawName(sessions[i]) == "" {
				b.Fatal("empty name")
			}
		}
	}
}

func TestNameCache_RawName_servesConcurrentCallersTheSameName(t *testing.T) {
	dir := sessionHome(t)
	writeSessionName(t, dir, 5800, "shared")
	s := state.Session{PID: 5800, CWD: "/home/u/Projects/Arachne"}

	c := &NameCache{}
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				got[i] = c.RawName(s)
			}
		}()
	}
	wg.Wait()
	for i, name := range got {
		if name != "shared" {
			t.Errorf("goroutine %d saw RawName = %q, want shared", i, name)
		}
	}
}
