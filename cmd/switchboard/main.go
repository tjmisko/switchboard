// Command switchboard is the daemon. It runs one long-lived process per
// user session, watches /proc for claude binaries, owns pidfds for instant
// death detection, listens to Hyprland's socket2 for window lifecycle, and
// serves an RPC socket for waybar + ctl.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/detect"
	"github.com/tjmisko/switchboard/internal/discovery"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
	"github.com/tjmisko/switchboard/internal/wm"
)

// permissionDecayTTL bounds how long a "permission" chip may stay latched once
// the transcript check can no longer confirm it is genuinely pending. The
// accurate path (a declined/answered prompt) clears within one reconcile tick;
// this TTL only governs the fail-soft case where the transcript is unreadable
// or inconclusive, so a stuck red chip still self-heals instead of nagging
// forever.
const permissionDecayTTL = 30 * time.Second

func main() {
	statePath := flag.String("state", defaultStatePath(), "path to state.json mirror")
	socketPath := flag.String("socket", defaultSocketPath(), "path to RPC unix socket")
	scanInterval := flag.Duration("scan-interval", 1*time.Second, "/proc scan interval")
	reconcileInterval := flag.Duration("reconcile-interval", 5*time.Second, "full reconcile interval")
	wmFlag := flag.String("wm", "auto", "WM backend: auto|hyprland|sway|i3|x11|none")
	terminalFlag := flag.String("terminal", "auto", "terminal backend: auto|wezterm|tmux|none")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	stack := detect.Detect(detect.Options{WM: *wmFlag, Terminal: *terminalFlag})
	caps := stack.Capabilities()
	log.Printf("backends: wm=%s terminal=%s observe=%t navigate=%t",
		caps.WM, caps.Terminal, caps.Observe, caps.Navigate)

	store := state.New(*statePath)
	store.SetCapabilities(caps)
	if err := store.Load(); err != nil {
		log.Printf("hydrate: %v (continuing)", err)
	}
	dropStaleSessions(store)

	procSrc := stack.OSProc
	term := stack.Terminal
	manager := stack.WM
	scanner := discovery.New()
	resolver := mapping.NewResolver(term, manager)

	onAgentAppeared := func(info proc.Info) {
		kind := discovery.Classify(info)
		log.Printf("%s pid=%d cwd=%s tty=%s discovered", kind, info.PID, info.CWD, info.TTY)
		sess := resolver.Resolve(ctx, info)
		sess.Agent = string(kind)
		store.Apply(func(m map[int]*state.Session) { m[sess.PID] = &sess })

		if err := procSrc.Watch(ctx, info.PID, func() {
			log.Printf("%s pid=%d died", kind, info.PID)
			store.Apply(func(m map[int]*state.Session) { delete(m, info.PID) })
			scanner.Forget(info.PID)
		}); err != nil {
			log.Printf("watch pid=%d: %v", info.PID, err)
		}
	}

	go func() {
		if err := scanner.Run(ctx, *scanInterval, onAgentAppeared); err != nil && ctx.Err() == nil {
			log.Printf("scanner: %v", err)
		}
	}()
	go runWMLoop(ctx, store, resolver, manager)
	go runReconciler(ctx, store, resolver, manager, stack, *reconcileInterval)

	server := rpc.New(store, *socketPath, term, manager)
	if err := os.MkdirAll(filepath.Dir(*socketPath), 0o755); err != nil {
		log.Fatalf("mkdir socket dir: %v", err)
	}
	log.Printf("switchboard listening on %s", *socketPath)
	if err := server.Serve(ctx); err != nil {
		log.Fatalf("rpc: %v", err)
	}
}

// dropStaleSessions removes hydrated sessions whose PID is gone or no longer
// looks like claude. Run once at startup, before any live discovery — the
// scanner will re-add survivors on the first tick.
func dropStaleSessions(store *state.Store) {
	now := time.Now()
	store.Apply(func(m map[int]*state.Session) {
		for pid := range m {
			info, err := proc.Read(pid)
			if err != nil || discovery.Classify(info) == discovery.AgentNone {
				delete(m, pid)
				continue
			}
			// StatusSince is in-memory only (json:"-"), so it loads as zero. Stamp
			// it to startup time: the attention self-heal compares transcript
			// resolution times against it, and a zero value would read every old
			// tool_result as "resolved after" — wrongly demoting a still-pending
			// prompt that was live across the restart. Startup time keeps such a
			// chip red until something genuinely resolves after the restart.
			if info := m[pid].Enrichment(); info != nil {
				info.StatusSince = now
			}
		}
	})
}

func runWMLoop(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager) {
	for ctx.Err() == nil {
		events, err := manager.Subscribe(ctx)
		if err != nil {
			log.Printf("wm subscribe: %v (retrying in 2s)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for evt := range events {
			handleWMEvent(ctx, store, resolver, evt)
		}
		// channel closed (connection EOF or ctx cancel) — loop will retry
	}
}

// handleWMEvent reacts to a neutral window event. Addresses arrive already
// normalized to Clients() form (the wm seam owns the Hyprland 0x quirk), so the
// daemon compares them directly against sess.Hyprland.Address.
func handleWMEvent(ctx context.Context, store *state.Store, resolver *mapping.Resolver, evt wm.Event) {
	switch evt.Kind {
	case wm.EventWindowClosed:
		// Drop any session living in the closed window. Covers the "user closed
		// the terminal while claude was running" case.
		store.Apply(func(m map[int]*state.Session) {
			for pid, sess := range m {
				if sess.Hyprland != nil && sess.Hyprland.Address == evt.Address {
					delete(m, pid)
				}
			}
		})
	case wm.EventFocusChanged:
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				if sess.Hyprland == nil {
					sess.Focused = false
					continue
				}
				sess.Focused = sess.Hyprland.Address == evt.Address
			}
		})
	case wm.EventLayoutChanged:
		// Something changed — kick a reconcile on any session that might match.
		// Cheap: just iterate live sessions and re-resolve.
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				resolver.Reconcile(ctx, sess)
			}
		})
	}
}

// runReconciler periodically re-resolves every session's wezterm + hyprland
// mapping and re-syncs the Focused flag against the current active window.
// Catches anything missed by event-driven updates (e.g. a session whose
// mapping was incomplete when first created, the initial focus state, or a
// hyprctl race).
func runReconciler(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	reconcileOnce(ctx, store, resolver, manager, stack)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcileOnce(ctx, store, resolver, manager, stack)
		}
	}
}

func reconcileOnce(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack) {
	// Re-publish capabilities every tick: the terminal locator is self-redetecting
	// (detect.NewAuto), so a terminal that came up after the daemon flips
	// terminal/navigate from their boot-race "none" values without a restart.
	store.SetCapabilities(stack.Capabilities())
	active, _ := manager.ActiveWindow(ctx)
	now := time.Now()
	store.Apply(func(m map[int]*state.Session) {
		for _, sess := range m {
			resolver.Reconcile(ctx, sess)
			if sess.Hyprland != nil {
				sess.Focused = sess.Hyprland.Address == active
			}
			// Refresh job-control suspension (Ctrl-Z). On ErrGone the procwatch
			// death callback will drop the session shortly; leave the last-known
			// value until then rather than flapping.
			if st, err := proc.State(sess.PID); err == nil {
				sess.Suspended = proc.Suspended(st)
			}
		}
		selfHealStaleAttention(m, now, permissionDecayTTL)
		selfHealStuckStatus(m, now)
	})
}

// selfHealStaleAttention releases a "permission" chip that Claude Code left
// latched. Declining a question — or interrupting a turn — fires no clearing
// hook (PostToolUse only fires on success; Stop not on interrupt), so the red
// state has nothing to release it. For each permission session it reads the tail
// of the transcript and asks whether the main conversation thread advanced past
// the prompt after StatusSince (when the chip went red): an assistant message or
// a user interrupt notice means it was answered/declined → demote to idle;
// otherwise it is still pending → stay red. Crucially, a bare tool_result is not
// treated as resolution — a background teammate/subagent or a sibling auto-tool
// keeps writing tool_results while the prompt waits, and counting them would flash
// the chip green the instant any concurrent work landed. A pending decision stays
// red even while subagents work.
//
// It runs inside the reconcile Apply, so it operates on the locked session map
// directly (no shared-pointer race) and folds into the tick's single persist.
// The bounded transcript read under the lock is consistent with the per-session
// /proc and WM I/O the same loop already performs.
func selfHealStaleAttention(m map[int]*state.Session, now time.Time, ttl time.Duration) {
	for _, sess := range m {
		if sess.Claude == nil || sess.Claude.Status != "permission" {
			continue
		}
		age := now.Sub(sess.Claude.StatusSince)
		st, _ := transcript.ResolutionState(sess.Claude.Transcript, sess.Claude.StatusSince, transcript.DefaultTailBytes)
		if shouldDecayPermission(st, age, ttl) {
			// This transition has no Claude Code hook behind it (a declined or
			// interrupted prompt fires none), so unlike the hook-driven edges it
			// would otherwise leave no trace. Log it with the deciding reason —
			// transcript-resolved vs the soft TTL fallback — so a self-healed red
			// chip is distinguishable from one that never latched.
			log.Printf("status: pid=%d session=%s decay permission->idle (reason=%s age=%s)",
				sess.PID, shortSessionID(sess.Claude.SessionID), decayReason(st), age.Round(time.Second))
			sess.Claude.Status = "idle"
			sess.Claude.StatusSince = now
		}
	}
}

// selfHealStuckStatus recovers the two non-permission status latches the hooks
// leave behind, both by reading the transcript tail (transcript.NewestSignal):
//
//   - idle → working: an orchestrator whose main turn ended (Stop → idle) and was
//     then woken by a background teammate fires no working hook, so the chip
//     stays orange while it recomputes. A conversational entry dated after the
//     chip went idle proves the session resumed.
//   - working → idle: interrupting a turn (Esc) fires no Stop hook, so the chip
//     stays green after the user stopped the agent. The "[Request interrupted by
//     user]" notice dated after the chip went working proves the turn was cut.
//
// A cheap stat short-circuits the common quiescent case: if nothing has been
// written since the chip's last transition, no signal can be newer than it, so
// the tail read is skipped. The read itself is bounded and runs inside the
// reconcile Apply, exactly like selfHealStaleAttention. Every flip re-stamps
// StatusSince, so the entry that triggered it is older than the new StatusSince
// on the next tick and cannot cause a reverse flip — no flapping.
//
// Deliberately keyed on the interrupt marker, not a no-activity TTL: a
// multi-minute tool run writes nothing to the transcript for the duration, so a
// TTL would wrongly decay a genuinely busy session; the marker has no such
// false-positive (a completed tool records "interrupted":false, not a text block).
func selfHealStuckStatus(m map[int]*state.Session, now time.Time) {
	for _, sess := range m {
		c := sess.Claude
		if c == nil || (c.Status != "idle" && c.Status != "working") {
			continue
		}
		fi, err := os.Stat(c.Transcript)
		if err != nil || !fi.ModTime().After(c.StatusSince) {
			continue
		}
		kind, ts, err := transcript.NewestSignal(c.Transcript, transcript.DefaultTailBytes)
		if err != nil || kind == transcript.SignalNone || !ts.After(c.StatusSince) {
			continue
		}
		switch {
		case c.Status == "idle" && kind == transcript.SignalActivity:
			log.Printf("status: pid=%d session=%s idle->working (reason=transcript-activity)", sess.PID, shortSessionID(c.SessionID))
			c.Status = "working"
			c.StatusSince = now
		case c.Status == "working" && kind == transcript.SignalInterrupt:
			log.Printf("status: pid=%d session=%s working->idle (reason=interrupt)", sess.PID, shortSessionID(c.SessionID))
			c.Status = "idle"
			c.StatusSince = now
		}
	}
}

// shouldDecayPermission decides whether a latched "permission" chip should fall
// back to idle. The transcript tail check is authoritative: a resolved prompt
// decays, a still-pending one is preserved (keep nagging). When the check is
// inconclusive — unreadable transcript, parse failure, no tool_use in the tail
// window — it fails soft to a TTL so a stuck chip still degenerates eventually
// instead of nagging forever.
func shouldDecayPermission(st transcript.PromptState, age, ttl time.Duration) bool {
	switch st {
	case transcript.StateResolved:
		return true
	case transcript.StatePending:
		return false
	default: // StateUnknown
		return age >= ttl
	}
}

// decayReason names why a permission chip was demoted, for the log trail:
// "resolved" — the transcript proved the prompt was answered/declined;
// "ttl" — the check was inconclusive and the soft timeout fired.
func decayReason(st transcript.PromptState) string {
	if st == transcript.StateResolved {
		return "resolved"
	}
	return "ttl"
}

// shortSessionID trims a Claude session UUID to its first segment for compact
// log lines while staying unique enough to grep. Empty stays "?".
func shortSessionID(id string) string {
	if id == "" {
		return "?"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func defaultStatePath() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "switchboard", "state.json")
	}
	return filepath.Join(os.Getenv("HOME"), ".cache", "switchboard", "state.json")
}

func defaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "switchboard.sock")
	}
	return fmt.Sprintf("/tmp/switchboard-%d.sock", os.Getuid())
}
