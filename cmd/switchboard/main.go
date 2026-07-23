// Command switchboard is the daemon. It runs one long-lived process per
// user session, watches /proc for claude binaries, owns pidfds for instant
// death detection, listens to Hyprland's socket2 for window lifecycle, and
// serves an RPC socket for waybar + ctl.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/tjmisko/switchboard/internal/detect"
	"github.com/tjmisko/switchboard/internal/discovery"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/projectname"
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
	historyDir := flag.String("history-dir", "", "activity-log directory (default $XDG_STATE_HOME/switchboard/history)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Activity log (opt-in via $XDG_CONFIG_HOME/switchboard/history.json). The sink
	// is best-effort and asynchronous, so recording an event never blocks the
	// state lock the hook/reconcile paths hold. Project labels are resolved here
	// (off the hot path) from a cached projectname config.
	histCfg := history.LoadConfig()
	if *historyDir != "" {
		histCfg.Dir = *historyDir
	}
	nameCfg := projectname.Load()
	histCfg.ResolveProject = func(cwd string) string { return projectname.CanonicalForDir(nameCfg, cwd) }
	sink := history.NewSink(histCfg)
	defer sink.Close()
	log.Printf("history: enabled=%t detail=%s dir=%s", sink.Enabled(), histCfg.Detail, sink.Dir())

	// One fanout Observer is the single source of truth for subagent detection,
	// shared by the reconcile loop and the SubagentStart/Stop hook handler (one
	// writer, two triggers). It seeds its per-session seen-set from the same history
	// dir the sink writes, so a daemon restart does not re-emit historical spawns.
	fanoutObs := fanout.NewObserver(sink.Dir())

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
	procSrc := stack.OSProc
	term := stack.Terminal
	manager := stack.WM
	// Stale-drop reads through the same osproc.Source the scanner and death-watch
	// use, so there is exactly one process-reading backend. Runs before the live
	// scan starts; the scanner re-adds survivors on the first tick.
	scanner := discovery.New(procSrc)
	dropStaleSessions(store, procSrc, sink, scanner.Forget)
	resolver := mapping.NewResolver(term, manager)

	onAgentAppeared := func(info osproc.Info) {
		kind := discovery.Classify(info)
		log.Printf("%s pid=%d cwd=%s tty=%s discovered", kind, info.PID, info.CWD, info.TTY)
		sess := resolver.Resolve(ctx, info)
		sess.Agent = string(kind)
		store.Apply(func(m map[int]*state.Session) { m[sess.PID] = &sess })
		// session_start bounds the session's first interval. The session id is not
		// known until the first hook fires, so this event carries only pid/agent/cwd.
		sink.Record(history.Event{Ts: time.Now(), Type: history.EventSessionStart,
			PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD})

		// The pidfd watch is the FAST path to session_end, not the only one: it lives
		// in this daemon's memory, so a restart or SIGKILL orphans it and the death it
		// would have reported is never observed. The reconciler's liveness sweep
		// backstops that loss (L1) and a registration failure below (L3); both funnel
		// through endSession, so whichever notices first closes the lane exactly once.
		if err := procSrc.Watch(ctx, info.PID, func() {
			log.Printf("%s pid=%d died", kind, info.PID)
			store.Apply(func(m map[int]*state.Session) {
				endSession(m, info.PID, sink, scanner.Forget, time.Now())
			})
		}); err != nil {
			log.Printf("watch pid=%d: %v (liveness sweep will close its lane)", info.PID, err)
		}
	}

	go func() {
		if err := scanner.Run(ctx, *scanInterval, onAgentAppeared); err != nil && ctx.Err() == nil {
			log.Printf("scanner: %v", err)
		}
	}()
	go runWMLoop(ctx, store, resolver, manager, sink)
	go runReconciler(ctx, store, resolver, manager, stack, *reconcileInterval, tun, sink, fanoutObs, scanner.Forget)

	server := rpc.New(store, *socketPath, term, manager)
	server.SetTuning(tun)
	server.SetHistory(sink)
	server.SetFanout(fanoutObs)
	if err := os.MkdirAll(filepath.Dir(*socketPath), 0o755); err != nil {
		log.Fatalf("mkdir socket dir: %v", err)
	}
	log.Printf("switchboard listening on %s", *socketPath)
	if err := server.Serve(ctx); err != nil {
		log.Fatalf("rpc: %v", err)
	}
}

// endSession closes one session's lane: it records the session_end that bounds
// the session's last interval, drops the session from the store map, and clears
// the scanner's seen-set entry so a recycled pid is re-discovered
// (decisions.md §12). It is the SINGLE writer of session_end, driven by three
// triggers: the pidfd death-watch (the fast path), the reconciler's liveness
// sweep (the durable backstop), and the startup stale-drop.
//
// Idempotent by map membership: whichever trigger fires first removes the
// session, so every later trigger finds nothing and records nothing — one death
// can never produce two session_end events (L5). The caller MUST hold the store
// lock (i.e. call this inside store.Apply). Reports whether it closed the lane.
func endSession(m map[int]*state.Session, pid int, sink *history.Sink, forget func(int), now time.Time) bool {
	s := m[pid]
	if s == nil {
		return false // already closed by another trigger
	}
	sink.Record(history.Event{Ts: now, Type: history.EventSessionEnd,
		SessionID: enrichmentID(s), PID: s.PID, Agent: s.Agent, CWD: s.CWD})
	delete(m, pid)
	if forget != nil {
		forget(pid)
	}
	return true
}

// sessionDead reports whether pid is DEFINITIVELY no longer this session's
// process: either it is gone (osproc.ErrGone), or the pid now belongs to a
// non-agent process (the kernel recycled it). Both are unambiguous ends.
//
// Any other read error reports "not dead". Such an error is transient, or comes
// from a backend that cannot answer at all (darwin's Read returns ErrUnsupported
// for every pid) — and a false session_end is far more damaging than closing a
// lane one tick late: it splits a running session into two lanes and permanently
// under-counts the second. Liveness is judged ONLY on positive evidence of
// death, never on inactivity (L4).
func sessionDead(src osproc.Source, pid int) bool {
	info, err := src.Read(pid)
	if errors.Is(err, osproc.ErrGone) {
		return true
	}
	if err != nil {
		return false // transient / unsupported backend — re-check next tick
	}
	return discovery.Classify(info) == discovery.AgentNone
}

// sweepDeadSessions closes the lane of every tracked session whose process is
// definitively gone. It is the DURABLE backstop for session_end, and the reason
// the daemon no longer depends on a death-watch surviving anything.
//
// The pidfd watch registered at discovery lives in this daemon's memory: a
// restart or SIGKILL orphans it, and the death it would have reported is never
// observed. A watch that failed to register never observes one either. In both
// cases nothing else would ever drop the session, and the reader stretches its
// final interval to `now` — the ghost lane this sweep exists to prevent
// (L1/L3, session-lifecycle-hazards.md). Polling here costs one process read per
// session per tick and depends on no prior state at all, so it self-heals across
// a restart within a single reconcile interval.
//
// Deleting from a map while ranging it is safe in Go. Runs inside store.Apply.
func sweepDeadSessions(m map[int]*state.Session, src osproc.Source, sink *history.Sink, forget func(int), now time.Time) {
	for pid := range m {
		if !sessionDead(src, pid) {
			continue
		}
		if endSession(m, pid, sink, forget, now) {
			log.Printf("liveness sweep: pid=%d gone, closed its lane", pid)
		}
	}
}

// dropStaleSessions removes hydrated sessions whose PID is gone or no longer
// looks like claude. Run once at startup, before any live discovery — the
// scanner will re-add survivors on the first tick. It reads through the
// osproc.Source, keeping discovery and stale-drop on a single process-reading
// backend.
//
// A stale session died while this daemon was DOWN, so no death-watch of ours
// ever existed to observe it. Recording its session_end here is what closes its
// lane; without it the lane stays open forever and the reader stretches its last
// interval to `now` — a ghost lane (L2, session-lifecycle-hazards.md).
func dropStaleSessions(store *state.Store, procSrc osproc.Source, sink *history.Sink, forget func(int)) {
	now := time.Now()
	store.Apply(func(m map[int]*state.Session) {
		for pid := range m {
			info, err := procSrc.Read(pid)
			if err == nil && discovery.Classify(info) != discovery.AgentNone {
				// StatusSince is in-memory only (json:"-"), so it loads as zero. Stamp
				// it to startup time: the attention self-heal compares transcript
				// resolution times against it, and a zero value would read every old
				// tool_result as "resolved after" — wrongly demoting a still-pending
				// prompt that was live across the restart. Startup time keeps such a
				// chip red until something genuinely resolves after the restart.
				if info := m[pid].Enrichment(); info != nil {
					info.StatusSince = now
				}
				continue
			}
			// Definitively dead (gone, or the pid recycled to a non-agent): record the
			// end that closes the lane. A non-definitive read error still drops the
			// stale entry, exactly as it always has, but must not fabricate an end for
			// a session that may well still be running.
			if errors.Is(err, osproc.ErrGone) || err == nil {
				endSession(m, pid, sink, forget, now)
			}
			delete(m, pid)
		}
	})
}

func runWMLoop(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, sink *history.Sink) {
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
			handleWMEvent(ctx, store, resolver, evt, sink)
		}
		// channel closed (connection EOF or ctx cancel) — loop will retry
	}
}

// handleWMEvent reacts to a neutral window event. Addresses arrive already
// normalized to Clients() form (the wm seam owns the Hyprland 0x quirk), so the
// daemon compares them directly against sess.Hyprland.Address.
func handleWMEvent(ctx context.Context, store *state.Store, resolver *mapping.Resolver, evt wm.Event, sink *history.Sink) {
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
		now := time.Now()
		store.Apply(func(m map[int]*state.Session) {
			applyFocus(m, evt.Address, sink, now)
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
func runReconciler(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, interval time.Duration, tun statustune.Tuning, sink *history.Sink, obs *fanout.Observer, forget func(int)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	rstate := newReconcileState(obs)
	reconcileOnce(ctx, store, resolver, manager, stack, tun, sink, rstate, forget)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcileOnce(ctx, store, resolver, manager, stack, tun, sink, rstate, forget)
		}
	}
}

func reconcileOnce(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, tun statustune.Tuning, sink *history.Sink, rstate *reconcileState, forget func(int)) {
	// Re-publish capabilities every tick: the terminal locator is self-redetecting
	// (detect.NewAuto), so a terminal that came up after the daemon flips
	// terminal/navigate from their boot-race "none" values without a restart.
	store.SetCapabilities(stack.Capabilities())
	active, _ := manager.ActiveWindow(ctx)
	now := time.Now()
	store.Apply(func(m map[int]*state.Session) {
		// Close the lanes of any session whose process is gone, BEFORE the per-tick
		// work below — a dead session earns none of it.
		sweepDeadSessions(m, stack.OSProc, sink, forget, now)
		for _, sess := range m {
			resolver.Reconcile(ctx, sess)
			// Refresh job-control suspension (Ctrl-Z). On ErrGone the sweep above has
			// already dropped the session, so this only ever sees a live pid; leave
			// the last-known value on any other read error rather than flapping. A
			// change is logged to history as a suspend/resume edge (it greys/un-greys
			// the chip in a timeline).
			if st, err := proc.State(sess.PID); err == nil {
				susp := proc.Suspended(st)
				if susp != sess.Suspended {
					evType := history.EventResume
					if susp {
						evType = history.EventSuspend
					}
					sink.Record(history.Event{Ts: now, Type: evType,
						SessionID: enrichmentID(sess), PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD})
				}
				sess.Suspended = susp
			}
			// Recompute the S dimension — in-flight subagent Tasks — from the main
			// transcript so the self-heals (and the wire/tooltip) see current
			// delegation, and emit fanout (subagent spawn/stop) + usage (token)
			// history events derived from the same read. Claude-only.
			if c := sess.Claude; c != nil {
				rstate.observe(sink, sess, c, now)
			}
		}
		// Re-sync focus against the active window (the backstop for any focus event
		// the live socket2 stream missed) and record a focus edge on a real change.
		// Runs after the resolve loop so every session's Hyprland address is current.
		applyFocus(m, active, sink, now)
		selfHealStaleAttention(m, now, tun, sink)
		selfHealStuckStatus(m, now, tun, sink)
		rstate.prune(m)
	})
}

// recordReconcileTransition mirrors a hookless reconciler status edge into the
// activity log, computing the closed interval's length from the still-current
// StatusSince (call it BEFORE re-stamping StatusSince). A no-op on a disabled sink.
func recordReconcileTransition(sink *history.Sink, sess *state.Session, c *state.AgentInfo, to, rule, reason string, now time.Time) {
	sink.Record(history.Event{
		Ts: now, Type: history.EventTransition,
		SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		From: c.Status, To: to, Rule: rule, Reason: reason,
		Subagents: c.InFlightSubagents, Pending: c.PendingTool,
		DurPrevMs: history.HeldMs(c.StatusSince, now),
	})
}

// applyFocus reconciles every session's Focused flag against the active window
// address and records a focus event when the focused AGENT session changes. It
// runs inside store.Apply (the caller holds the state lock): it reads the prior
// Focused flags to recover which agent session was focused, flips them to match
// activeAddr, and — only on a real change — sink.Records an EventFocus. The
// event's SessionID is the newly-focused agent session, or EMPTY when focus left
// every agent window (a non-agent window, or none, is now active); that empty-
// SessionID signal is what the timeline deriver uses to close a focus span.
//
// Change is keyed on the focused session id, not the pid, so a non-agent window
// and an agent whose first hook has not yet assigned a SessionID both read as
// "no agent focused" (empty) and collapse to one state — the reconcile backstop
// re-emits with the real id once the hook lands. activeAddr is the normalized
// active-window address ("" → no/unknown window, so all sessions unfocus). The
// event carries only ids/pid/agent (no cwd) — focus is minimal-safe.
func applyFocus(m map[int]*state.Session, activeAddr string, sink *history.Sink, now time.Time) {
	prevID := ""
	for _, sess := range m {
		if sess.Focused {
			prevID = enrichmentID(sess)
			break
		}
	}
	newID, newPID, newAgent := "", 0, ""
	for _, sess := range m {
		focused := activeAddr != "" && sess.Hyprland != nil && sess.Hyprland.Address == activeAddr
		sess.Focused = focused
		if focused {
			newID, newPID, newAgent = enrichmentID(sess), sess.PID, sess.Agent
		}
	}
	if newID == prevID {
		return
	}
	sink.Record(history.Event{
		Ts: now, Type: history.EventFocus,
		SessionID: newID, PID: newPID, Agent: newAgent,
	})
}

// enrichmentID returns the session's agent session id (the stable history join
// key), or "" before any hook has supplied it.
func enrichmentID(s *state.Session) string {
	if info := s.Enrichment(); info != nil {
		return info.SessionID
	}
	return ""
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
func selfHealStaleAttention(m map[int]*state.Session, now time.Time, tun statustune.Tuning, sink *history.Sink) {
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
		recordReconcileTransition(sink, sess, c, exit, rule, reason, now)
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
func selfHealStuckStatus(m map[int]*state.Session, now time.Time, tun statustune.Tuning, sink *history.Sink) {
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
				logStuck(sink, sess, c, state.StatusDelegating, statustune.RuleDelegating, "idle with subagents in flight", now)
				c.Status = state.StatusDelegating
				c.StatusSince = now
				continue
			case c.Status == state.StatusDelegating && c.InFlightSubagents == 0:
				logStuck(sink, sess, c, state.StatusIdle, statustune.RuleDrained, "subagents drained", now)
				c.Status = state.StatusIdle
				c.StatusSince = now
				continue
			case c.Status == state.StatusDelegating:
				continue // still delegating; nothing to recover
			}
		}
		// The silent abort (docs/timing-hazards.md H9): a prompt submitted and
		// interrupted before its first token fires no Stop hook AND writes no
		// interrupt marker, so neither event stream ever demotes the chip — it
		// would stay green until the next manual prompt. The recovery is a third
		// stream: the pane's own title, where Claude Code animates a spinner
		// while a turn runs and parks the static idle glyph while waiting at the
		// prompt. The resolver re-samples it (stamping TitleAt) every tick, so a
		// title that (a) was sampled after the chip went working, (b) shows the
		// idle glyph, and (c) has had IdleTitleGrace to flip past the edge lag,
		// proves no turn is running. Runs before the mtime pre-gate below — the
		// transcript is silent in exactly this failure. Claude-only (codex paints
		// no glyph), never on a suspended session (frozen title, and the overlay
		// already de-emphasizes it). A false demote (broken title updates)
		// self-corrects: the turn's next transcript write re-greens the chip via
		// resume-activity.
		if tun.IdleTitleDemotionEnabled && c.Status == state.StatusWorking &&
			sess.Agent == state.AgentKindClaude && !sess.Suspended &&
			sess.Wezterm != nil && sess.Wezterm.TitleAt.After(c.StatusSince) &&
			titleShowsIdleGlyph(sess.Wezterm.Title, tun.IdleTitleGlyphs) &&
			now.Sub(c.StatusSince) >= tun.IdleTitleGrace {
			logStuck(sink, sess, c, state.StatusIdle, statustune.RuleIdleTitle, "idle title glyph on a working chip", now)
			c.Status = state.StatusIdle
			c.StatusSince = now
			continue
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
			logStuck(sink, sess, c, state.StatusWorking, statustune.RuleResumeActivity, "transcript activity after idle", now)
			c.Status = state.StatusWorking
			c.StatusSince = now
		case c.Status == state.StatusWorking && kind == transcript.SignalInterrupt:
			logStuck(sink, sess, c, state.StatusIdle, statustune.RuleInterrupt, "interrupt notice after working", now)
			c.Status = state.StatusIdle
			c.StatusSince = now
		}
	}
}

// logStuck emits the decision log for a reconciler-driven (hookless) status edge,
// capturing the full observed state so a wrong color can be traced to its inputs,
// and mirrors the edge into the durable activity log. Called BEFORE the caller
// re-stamps StatusSince, so the closed interval's length is correct.
func logStuck(sink *history.Sink, sess *state.Session, c *state.AgentInfo, to, rule, reason string, now time.Time) {
	statustune.Decision{
		PID: sess.PID, Session: shortSessionID(c.SessionID),
		From: c.Status, To: to, Rule: rule, Reason: reason,
		Subagents: c.InFlightSubagents, Pending: c.PendingTool,
		Age: now.Sub(c.StatusSince),
	}.Log()
	recordReconcileTransition(sink, sess, c, to, rule, reason, now)
}

// titleShowsIdleGlyph reports whether a pane title's first rune is one of the
// configured idle glyphs (the agent parked at its prompt). Anything else — a
// spinner frame, a shell title, an empty string — is "no signal", never a
// demotion: the H9 recovery must key on positive evidence of idleness, so a
// terminal that does not carry agent titles simply leaves the rule inert.
func titleShowsIdleGlyph(title, glyphs string) bool {
	title = strings.TrimSpace(title)
	if title == "" || glyphs == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(title)
	return strings.ContainsRune(glyphs, r)
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
