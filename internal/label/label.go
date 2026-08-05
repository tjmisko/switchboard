// Package label centralizes how a session is named for display. It sources the
// raw human name (preferring the authoritative Claude session name on disk),
// then applies project prefixing via internal/projectname so the bottom-bar
// chips and `switchboard-ctl` list/pick agree on a single label.
//
// RawName and Chip read the disk on every call, which is what a one-shot caller
// wants. A caller that names sessions in a loop should hold a NameCache and use
// its methods instead.
package label

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
)

// spinnerPrefixes are the leading glyphs Claude Code writes onto the wezterm
// window title while working; we strip them so the bare name shows.
var spinnerPrefixes = []string{"✳ ", "⠂ ", "⠐ ", "⠁ ", "⠈ ", "⠠ ", "⠄ ", "⡀ ", "⢀ "}

// RawName picks the human name for a session before project prefixing:
//  1. the Claude session name from ~/.claude/sessions/<pid>.json (what `/name`
//     and the launcher both set — authoritative and terminal-independent);
//  2. the wezterm window title with any spinner glyph stripped;
//  3. the cwd basename;
//  4. "pid N" as a last resort.
func RawName(s state.Session) string {
	return rawName(s, claudeSessionName)
}

// Chip returns the project-prefixed, de-duplicated label for a session's chip.
func Chip(cfg projectname.Config, s state.Session) string {
	return projectname.ResolveForDir(cfg, s.CWD, RawName(s))
}

// rawName is RawName's fallback chain with the disk lookup injected, so the
// cached and uncached paths cannot drift apart.
func rawName(s state.Session, claudeName func(pid int) string) string {
	if n := claudeName(s.PID); n != "" {
		return n
	}
	if s.Wezterm != nil && s.Wezterm.WindowTitle != "" {
		title := s.Wezterm.WindowTitle
		for _, p := range spinnerPrefixes {
			if rest, ok := strings.CutPrefix(title, p); ok {
				title = rest
				break
			}
		}
		return title
	}
	if s.CWD != "" {
		return filepath.Base(s.CWD)
	}
	return fmt.Sprintf("pid %d", s.PID)
}

// claudeSessionName reads the `name` field from ~/.claude/sessions/<pid>.json,
// returning "" when the file is absent, unreadable, or carries no name.
func claudeSessionName(pid int) string {
	path, ok := sessionPath(pid)
	if !ok {
		return ""
	}
	return readSessionName(path)
}

// sessionPath is the session file for a pid, and whether one can exist at all
// (a non-positive pid or an unknowable home has no file to name it).
func sessionPath(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".claude", "sessions", strconv.Itoa(pid)+".json"), true
}

// readSessionName pulls the trimmed `name` out of a session file, "" on any
// failure — absent, unreadable, malformed, or simply unnamed.
func readSessionName(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var rec struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &rec) != nil {
		return ""
	}
	return strings.TrimSpace(rec.Name)
}

// NameCache memoizes RawName's disk lookup per pid, re-reading a session's file
// only when its stamp has moved. Its zero value is ready to use, and a nil
// *NameCache behaves exactly like the uncached package functions, so a caller
// with no cache to hand (or a test not exercising one) can pass nil.
//
// # Why a cache exists at all
//
// RawName reads and unmarshals ~/.claude/sessions/<pid>.json on every call, and
// two callers call it in a loop:
//
//   - switchboard-waybar's renderSlot names EVERY session in the snapshot, not
//     just its own, because each slot must compute the same barlayout.Fit budget
//     for the abbreviation to agree across chips. The bar declares ten slot
//     modules, each its own process, so one snapshot with N sessions costs 10*N
//     reads — ~130 at this machine's usual session count.
//   - the daemon's observeLabel names every session once per reconcile tick, and
//     it does so inside the store lock, where the whole point of the surrounding
//     work is that nothing should be touching the disk.
//
// # Why it lives here rather than in each caller
//
// The projectname config cache (see nameConfig in cmd/switchboard-waybar) went
// the other way — into the single caller with a hot loop — precisely because
// nobody else would have benefited. That does not hold here: there are two hot
// callers in two different binaries, and open-coding the same stamp bookkeeping
// twice invites the two copies to drift.
//
// What that comment was actually protecting against was package-level mutable
// state hidden behind a bare function, where a future concurrent caller would
// race silently and a write-then-read-back would start depending on stat
// granularity. An explicit value the caller owns avoids both: RawName, Chip and
// claudeSessionName keep reading the disk unconditionally for the one-shot
// caller (switchboard-ctl), the cache's lifetime is visible at every call site,
// and the mutex means a concurrent caller is safe rather than silently racy.
//
// # Why the stamp, and not load-once
//
// The name can change under a live session — `/name` rewrites the file — and the
// bar is expected to show the new name on the next snapshot. So the entry is
// keyed by the file's (path, mtime, size), the same shape nameConfig uses: path
// so a caller that moves HOME mid-process (tests do) is not served the old
// home's answer, mtime for the ordinary rewrite, size to catch a same-mtime
// rewrite on a coarse-granularity filesystem. Claude Code rewrites this file for
// its own reasons too (it carries a status and an updatedAt), which is what
// rules out caching the name for the session's lifetime — but measured against
// this machine's live sessions the file moves about once per six session-minutes,
// against a render cadence of ~1s, so a miss is rare and a miss costs exactly
// what every call used to cost.
type NameCache struct {
	mu      sync.Mutex
	entries map[int]nameEntry
}

// nameEntry is one pid's memoized name plus the stamp of the file it came from.
type nameEntry struct {
	path    string
	modTime time.Time
	size    int64
	name    string
}

// maxNameCacheEntries bounds the map so a long-lived process does not accumulate
// an entry per pid that has ever appeared in a snapshot. Sessions come and go
// while the daemon and the bar run for days, and an entry is only ~64 bytes, but
// unbounded is unbounded. Overflow drops the whole map rather than evicting a
// victim: that is O(1), needs no recency bookkeeping, and costs only a cold
// re-read of the live set — which at a live count two orders of magnitude below
// the bound happens roughly never. A caller with more than this many LIVE
// sessions degrades to the uncached behavior, correctly, rather than misbehaving.
const maxNameCacheEntries = 512

// RawName is RawName with the disk lookup memoized.
func (c *NameCache) RawName(s state.Session) string {
	if c == nil {
		return RawName(s)
	}
	return rawName(s, c.claudeSessionName)
}

// Chip is Chip with the disk lookup memoized.
func (c *NameCache) Chip(cfg projectname.Config, s state.Session) string {
	return projectname.ResolveForDir(cfg, s.CWD, c.RawName(s))
}

// claudeSessionName serves the memoized name for a pid, re-reading only when the
// file's identity or stamp has moved. One stat replaces a read plus an unmarshal
// on the overwhelming majority of calls.
func (c *NameCache) claudeSessionName(pid int) string {
	path, ok := sessionPath(pid)
	if !ok {
		return ""
	}
	// A missing file (a Codex session, or a claude that has not written one yet)
	// stamps as the zero mtime and size, which is itself a usable key: the "no
	// name on disk" answer stays cached until the file shows up, at which point
	// the stamp goes non-zero and we read it.
	var modTime time.Time
	var size int64
	if fi, err := os.Stat(path); err == nil {
		modTime, size = fi.ModTime(), fi.Size()
	}

	c.mu.Lock()
	e, hit := c.entries[pid]
	c.mu.Unlock()
	if hit && e.path == path && e.size == size && e.modTime.Equal(modTime) {
		return e.name
	}

	// Deliberately read OUTSIDE the lock. Two callers racing the same cold pid
	// both read and both store the same answer, which is harmless; holding a
	// mutex across a file read is the thing this whole change exists to stop.
	name := readSessionName(path)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[int]nameEntry)
	}
	if len(c.entries) >= maxNameCacheEntries {
		clear(c.entries)
	}
	c.entries[pid] = nameEntry{path: path, modTime: modTime, size: size, name: name}
	return name
}
