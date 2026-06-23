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
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/transcript"
	"github.com/tjmisko/switchboard/internal/wm"
)

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

	// tun holds every status-color knob (statustune.Tuning). It is built once here
	// and threaded into both decision sites — the RPC hook gate and the reconciler
	// — so all color behavior is tuned from one place. Defaults encode the §8
	// recommendations; override fields here to retune without touching the logic.
	tun := statustune.Default()

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
	go runReconciler(ctx, store, resolver, manager, stack, *reconcileInterval, tun)

	server := rpc.New(store, *socketPath, term, manager)
	server.SetTuning(tun)
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
func runReconciler(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, interval time.Duration, tun statustune.Tuning) {
	t := time.NewTicker(interval)
	defer t.Stop()
	reconcileOnce(ctx, store, resolver, manager, stack, tun)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcileOnce(ctx, store, resolver, manager, stack, tun)
		}
	}
}

func reconcileOnce(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, tun statustune.Tuning) {
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
			// Recompute the S dimension — in-flight subagent Tasks — from the main
			// transcript so the self-heals (and the wire/tooltip) see current
			// delegation. Claude-only; a quiet read failure leaves the last count.
			if c := sess.Claude; c != nil && c.Transcript != "" {
				if n, err := transcript.InFlightTasks(c.Transcript, tun.TailBytes); err == nil {
					c.InFlightSubagents = n
				}
			}
		}
		selfHealStaleAttention(m, now, tun)
		selfHealStuckStatus(m, now, tun)
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
func selfHealStaleAttention(m map[int]*state.Session, now time.Time, tun statustune.Tuning) {
	for _, sess := range m {
		c := sess.Claude
		if c == nil || c.Status != state.StatusPermission {
			continue
		}
		age := now.Sub(c.StatusSince)
		kind, err := transcript.ResolveKind(c.Transcript, c.StatusSince, tun.TailBytes)
		exit, rule, reason, ok := permissionExit(kind, err != nil, age, c.InFlightSubagents, tun)
		if !ok {
			continue // still pending (or too soon to give up) → keep red, silently
		}
		// This transition has no Claude Code hook behind it (a declined or
		// interrupted prompt fires none), so unlike the hook-driven edges it would
		// otherwise leave no trace. The decision log records WHICH rule fired and
		// the full observed state, so a self-healed red chip — and its exit color —
		// is fully reconstructable from the journal.
		statustune.Decision{
			PID: sess.PID, Session: shortSessionID(c.SessionID),
			From: state.StatusPermission, To: exit, Rule: rule, Reason: reason,
			Subagents: c.InFlightSubagents, Pending: c.PendingTool, Age: age,
		}.Log()
		c.Status = exit
		c.StatusSince = now
		c.PendingTool = ""
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
func selfHealStuckStatus(m map[int]*state.Session, now time.Time, tun statustune.Tuning) {
	for _, sess := range m {
		c := sess.Claude
		if c == nil {
			continue
		}
		// Delegating (cases 5/14, fixes complaint #2): an idle main thread with
		// subagents still in flight is working-by-proxy → render green. This is
		// decided from the S dimension (recomputed in reconcileOnce), NOT from a
		// transcript-activity read, and so runs BEFORE the mtime pre-gate below:
		// while a teammate runs, the MAIN transcript is quiet (the subagent writes
		// its own), so the pre-gate would skip it and the chip would lag orange.
		// Revert to idle once the last teammate drains.
		if tun.DelegatingEnabled {
			switch {
			case c.Status == state.StatusIdle && c.InFlightSubagents > 0:
				logStuck(sess, c, state.StatusDelegating, statustune.RuleDelegating, "idle with subagents in flight", now)
				c.Status = state.StatusDelegating
				c.StatusSince = now
				continue
			case c.Status == state.StatusDelegating && c.InFlightSubagents == 0:
				logStuck(sess, c, state.StatusIdle, statustune.RuleDrained, "subagents drained", now)
				c.Status = state.StatusIdle
				c.StatusSince = now
				continue
			case c.Status == state.StatusDelegating:
				continue // still delegating; nothing to recover
			}
		}
		if c.Status != state.StatusIdle && c.Status != state.StatusWorking {
			continue
		}
		// A cheap stat short-circuits the quiescent case: if nothing was written
		// since the chip transitioned, no signal can be newer than it. (Delegating
		// is handled above precisely because this gate would skip it.)
		fi, err := os.Stat(c.Transcript)
		if err != nil || !fi.ModTime().After(c.StatusSince) {
			continue
		}
		kind, ts, err := transcript.NewestSignal(c.Transcript, tun.TailBytes)
		if err != nil || kind == transcript.SignalNone || !ts.After(c.StatusSince) {
			continue
		}
		switch {
		case c.Status == state.StatusIdle && kind == transcript.SignalActivity:
			logStuck(sess, c, state.StatusWorking, statustune.RuleResumeActivity, "transcript activity after idle", now)
			c.Status = state.StatusWorking
			c.StatusSince = now
		case c.Status == state.StatusWorking && kind == transcript.SignalInterrupt:
			logStuck(sess, c, state.StatusIdle, statustune.RuleInterrupt, "interrupt notice after working", now)
			c.Status = state.StatusIdle
			c.StatusSince = now
		}
	}
}

// logStuck emits the decision log for a reconciler-driven (hookless) status edge,
// capturing the full observed state so a wrong color can be traced to its inputs.
func logStuck(sess *state.Session, c *state.AgentInfo, to, rule, reason string, now time.Time) {
	statustune.Decision{
		PID: sess.PID, Session: shortSessionID(c.SessionID),
		From: c.Status, To: to, Rule: rule, Reason: reason,
		Subagents: c.InFlightSubagents, Pending: c.PendingTool,
		Age: now.Sub(c.StatusSince),
	}.Log()
}

// permissionExit decides whether — and to which color — a latched "permission"
// chip should exit, given the transcript resolution kind, whether the transcript
// was unreadable, how long it has been red, the in-flight subagent count, and the
// tuning. It is the pure core of selfHealStaleAttention (kept separate so the
// §5 case table is unit-testable). ok=false keeps the chip red. The cases:
//
//   - unreadable transcript: keep red until the TTL backstop fires (case 15;
//     observed 0× — the accurate path resolves first). The exit is the interrupt
//     color (your turn) since an abandoned prompt is most like a declined one.
//   - resumed (assistant message): approved → turn resumed → green, DIRECTLY,
//     with no orange bounce (case 9, P3).
//   - interrupted (Esc/decline) with subagents in flight: work continues →
//     green/delegating (case 11, Q3); otherwise your turn → orange (case 10).
//   - none (readable but nothing resolved it): still pending → keep red.
func permissionExit(kind transcript.ResolutionKind, unreadable bool, age time.Duration, subagents int, tun statustune.Tuning) (exit, rule, reason string, ok bool) {
	if unreadable {
		if age >= tun.PermissionDecayTTL {
			return tun.InterruptExitStatus, statustune.RuleTTLBackstop, "transcript unreadable; ttl elapsed", true
		}
		return "", "", "", false
	}
	switch kind {
	case transcript.ResolutionResumed:
		return tun.ResumeExitStatus, statustune.RuleApproveResume, "transcript: turn resumed", true
	case transcript.ResolutionInterrupted:
		if subagents > 0 {
			return tun.EscWithTeammatesStatus, statustune.RuleDeclineDelegating, "interrupt with subagents in flight", true
		}
		return tun.InterruptExitStatus, statustune.RuleDeclineIdle, "transcript: declined/interrupted", true
	default: // ResolutionNone
		return "", "", "", false
	}
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
