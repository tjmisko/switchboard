// Package label centralizes how a session is named for display. It sources the
// raw human name (preferring the authoritative Claude session name on disk),
// then applies project prefixing via internal/projectname so the bottom-bar
// chips and `switchboard-ctl` list/pick agree on a single label.
//
// It names the two things a renderer shows: the SESSION (RawName/Chip) and, when
// the session is red, the WRITERS inside it that are blocked on a permission
// prompt (BlockedWriters).
//
// RawName, Chip and BlockedWriters read the disk on every call, which is what a
// one-shot caller wants. A caller that names sessions in a loop should hold a
// NameCache and use its methods instead.
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
	"github.com/tjmisko/switchboard/internal/transcript"
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

// BlockedWriters names the writers of s currently blocked on a permission prompt,
// as one display string a renderer drops straight into a tooltip or a status row:
//
//	"escalate-cleanup"                   one teammate is blocked
//	"main, escalate-cleanup"             case 18 — two writers blocked at once
//	"escalate-cleanup, probe, scan +2"   capped, with the overflow counted
//
// It exists because a red chip alone says only "something in this session needs a
// decision". A session is 1 + N concurrent writers, so with teammates running the
// user otherwise has to switch to the pane just to learn WHICH one is stuck —
// which is the whole action the red was supposed to prompt.
//
// # What it reads
//
// The blocked set is `claude.pending_writers` off the wire, which is already
// SORTED and already spells the main thread "main" (state.PendingWriterMain), so
// this never ranges a map and the output is stable across snapshots. Each
// non-main key is a bare subagent agent_id, resolved to a human name through the
// session's own transcript path — the id's meta.json — by transcript.
// SubagentDisplayName. An id that resolves to nothing degrades to a short prefix
// of itself rather than disappearing: the count of blocked writers is never a lie.
//
// # When it says nothing
//
// "" when no writer is blocked, and — deliberately — when the ONLY blocked writer
// is the main thread of a session with no teammates in flight. That is the solo
// case, where "main" restates what the red chip already means. The moment a
// teammate exists the main thread stops being the obvious suspect, so the word
// starts carrying information and is shown.
//
// Reads the disk once per blocked writer. Use (*NameCache).BlockedWriters in a
// render loop.
func BlockedWriters(s state.Session) string {
	return blockedWriters(s, transcript.SubagentDisplayName)
}

// maxNamedBlockedWriters caps how many writers are spelled out before the rest
// collapse to "+N". Bar real estate is contended and a name is only actionable if
// it can be read; past a few, "several writers are blocked" is the real message
// and the exact roster is what the pane is for.
const maxNamedBlockedWriters = 3

// blockedWriters is BlockedWriters with the per-id disk lookup injected, so the
// cached and uncached paths cannot drift apart (the same shape rawName uses).
func blockedWriters(s state.Session, displayName func(metaPath string) string) string {
	info := s.Enrichment()
	if info == nil || len(info.PendingWriters) == 0 {
		return ""
	}
	// Solo main thread: the red chip already says this. See the doc comment.
	if len(info.PendingWriters) == 1 && info.PendingWriters[0] == state.PendingWriterMain && info.InFlightSubagents == 0 {
		return ""
	}

	named := make([]string, 0, maxNamedBlockedWriters)
	for i, w := range info.PendingWriters { // already sorted on the wire
		if i == maxNamedBlockedWriters {
			return strings.Join(named, ", ") + fmt.Sprintf(" +%d", len(info.PendingWriters)-i)
		}
		named = append(named, writerName(info.Transcript, w, displayName))
	}
	return strings.Join(named, ", ")
}

// writerName resolves one pending-writer key to the word shown for it: "main"
// passes through untouched (the main thread is not a spawn and has no meta), a
// subagent id becomes its meta's name/agentType, and anything unresolvable falls
// back to a short prefix of the id.
//
// Two writers of the same agentType render identically ("Explore, Explore"). That
// is left alone on purpose: it is a truthful count, and disambiguating them with
// hex ids would add characters the user cannot match to anything on screen.
func writerName(mainTranscript, writer string, displayName func(metaPath string) string) string {
	if writer == state.PendingWriterMain {
		return state.PendingWriterMain
	}
	if n := displayName(transcript.SubagentMetaPath(mainTranscript, writer)); n != "" {
		return n
	}
	return shortWriterID(writer)
}

// shortWriterIDRunes is how much of an unresolvable agent id survives as a
// fallback label — enough to tell two blocked writers apart in the same tooltip,
// short enough not to spend a chip's worth of hover on a hex string.
const shortWriterIDRunes = 8

// shortWriterID truncates an agent id for display, marking the cut with an
// ellipsis so it does not read as a complete identifier. Counts runes, not bytes:
// a subagent id embeds a user-supplied name, which need not be ASCII.
func shortWriterID(id string) string {
	r := []rune(id)
	if len(r) <= shortWriterIDRunes {
		return id
	}
	return string(r[:shortWriterIDRunes]) + "…"
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
//
// # Why it also memoizes subagent names
//
// BlockedWriters resolves each blocked writer through its
// subagents/agent-<id>.meta.json, and it is called from the same two hot loops:
// the bar renders a tooltip for its slot's session on every emission, ten slot
// processes deep. Left uncached that is a read + unmarshal per blocked writer per
// emission, for the entire life of a prompt — which is precisely the per-render
// I/O the session-name half of this cache exists to remove. It rides in the same
// value, under the same lock, with the same stamp discipline (and the same
// read-outside-the-lock rule), so there is one thing for a caller to hold and one
// bounded map policy to reason about.
//
// The subagent stamp is a near-permanent cache hit rather than a probable one: a
// meta.json is written once at spawn and never rewritten, so after the first read
// every subsequent call is a bare stat.
type NameCache struct {
	mu      sync.Mutex
	entries map[int]nameEntry
	// writers memoizes subagent display names, keyed by the meta.json's PATH (so
	// nameEntry.path is redundant here and left unset — the key already carries it).
	writers map[string]nameEntry
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

// Chip is Chip with the disk lookup memoized. dirs memoizes the other half — the
// cwd -> project resolution — and may be nil, which resolves it uncached.
//
// The two caches are separate values because their lifetimes are: this one is
// process-wide and stamp-validated, while a *projectname.DirCache is scoped to a
// single render on purpose. See its doc comment.
func (c *NameCache) Chip(cfg projectname.Config, dirs *projectname.DirCache, s state.Session) string {
	return dirs.ResolveForDir(cfg, s.CWD, c.RawName(s))
}

// BlockedWriters is BlockedWriters with the per-writer meta.json lookup memoized.
func (c *NameCache) BlockedWriters(s state.Session) string {
	if c == nil {
		return BlockedWriters(s)
	}
	return blockedWriters(s, c.subagentDisplayName)
}

// subagentDisplayName serves the memoized display name for one subagent meta
// path, re-reading only when the file's stamp has moved. A meta.json is written
// once at spawn, so past the first call this is a stat and nothing else.
//
// An empty path — the main thread, or a session with no transcript — never
// reaches the cache: there is no file, and no entry worth keeping.
func (c *NameCache) subagentDisplayName(metaPath string) string {
	if metaPath == "" {
		return ""
	}
	// A missing meta stamps as the zero mtime and size, which is a usable key in
	// its own right: the "not on disk yet" answer stays cached until the file
	// appears, at which point the stamp goes non-zero and we read it.
	var modTime time.Time
	var size int64
	if fi, err := os.Stat(metaPath); err == nil {
		modTime, size = fi.ModTime(), fi.Size()
	}

	c.mu.Lock()
	e, hit := c.writers[metaPath]
	c.mu.Unlock()
	if hit && e.size == size && e.modTime.Equal(modTime) {
		return e.name
	}

	// Read OUTSIDE the lock, for the reason claudeSessionName does: two callers
	// racing the same cold path both read and both store the same answer, which is
	// harmless, where holding the mutex across a file read is not.
	name := transcript.SubagentDisplayName(metaPath)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writers == nil {
		c.writers = make(map[string]nameEntry)
	}
	if len(c.writers) >= maxNameCacheEntries {
		clear(c.writers)
	}
	c.writers[metaPath] = nameEntry{modTime: modTime, size: size, name: name}
	return name
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
