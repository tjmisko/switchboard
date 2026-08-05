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
	"sync"
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
	"github.com/tjmisko/switchboard/internal/terminal"
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
	// max_bytes is logged because memory sampling raises its default into the
	// gigabytes: a disk commitment that large should never be made silently.
	log.Printf("history: enabled=%t detail=%s memory=%t retain_days=%d max_bytes=%s dir=%s",
		sink.Enabled(), histCfg.Detail, histCfg.Memory, histCfg.RetainDays,
		history.HumanBytes(histCfg.MaxBytes), sink.Dir())

	// Per-session memory sampling follows the history opt-in: the durable series
	// is the point of the reading, so a user who has not opted in pays none of
	// its /proc cost.
	var memSampler *memorySampler
	if histCfg.Enabled && histCfg.Memory {
		memSampler = newMemorySampler()
	}

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
		headless := kind == discovery.AgentClaude && discovery.IsHeadless(info)
		suffix := ""
		if headless {
			suffix = " headless"
		}
		log.Printf("%s pid=%d cwd=%s tty=%s discovered%s", kind, info.PID, info.CWD, info.TTY, suffix)
		sess := resolver.Resolve(ctx, info)
		sess.Agent = string(kind)
		sess.Headless = headless
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
	// One turn shared by both resolve producers, so an older enumeration can never
	// land after a newer one. See resolveTurn.
	turn := &resolveTurn{}
	go runWMLoop(ctx, store, resolver, manager, sink, procSrc, scanner.Forget, turn)
	go runReconciler(ctx, store, resolver, manager, stack, *reconcileInterval, tun, sink, fanoutObs, memSampler, scanner.Forget, turn)

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
// (decisions.md §12). It is the SINGLE writer of session_end, driven by four
// triggers: the pidfd death-watch (the fast path), the reconciler's liveness
// sweep (the durable backstop), the startup stale-drop, and the WM's
// window-closed event. Nothing else may remove a session from the store map —
// a bare delete writes no end, so the lane can only ever close at the reader's
// bound, and the sweep can never see the pid again because it only ranges the
// map (L7).
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

// layoutDebounce is the window a burst of layout events is coalesced into.
// Three raw Hyprland events map to EventLayoutChanged — movewindowv2,
// windowtitlev2, openwindow (internal/wm/hyprland.go) — and each one re-resolves
// EVERY session, which costs a full terminal + WM enumeration. windowtitlev2 is
// the hazard: a running agent CLI repaints its pane title as it works, so an
// uncoalesced stream multiplies that enumeration by the title rate.
//
// Rate limit, NOT a plain trailing-edge debounce: the timer is armed by the
// first event of a burst and not re-armed by the rest, so it always fires within
// one window of the burst starting. A re-arming debounce would have no maximum
// wait at all — under events spaced closer than the window it never fires — and
// the staleness bound would quietly become the reconcile interval. See
// drainWMEvents.
//
// So the cost really is at most one window of staleness on a chip's workspace,
// and the burst's last state still lands: an event arriving after a firing finds
// the timer disarmed and arms it again.
const layoutDebounce = 200 * time.Millisecond

func runWMLoop(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, sink *history.Sink, src osproc.Source, forget func(int), turn *resolveTurn) {
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
		drainWMEvents(ctx, store, resolver, events, sink, src, forget, turn, layoutDebounce)
		// channel closed (connection EOF or ctx cancel) — loop will retry
	}
}

// drainWMEvents dispatches one subscription's events until the stream closes,
// coalescing EventLayoutChanged (see layoutDebounce) and passing everything else
// straight through.
//
// ONLY layout is delayed. EventFocusChanged drives the chip highlight and the
// click-to-focus round trip, and EventWindowClosed drives session teardown;
// debouncing either would be a user-visible regression, not an optimization.
//
// debounce is a parameter rather than the layoutDebounce constant so tests can
// drive the coalescing without sleeping a real 200ms per case.
func drainWMEvents(ctx context.Context, store *state.Store, resolver *mapping.Resolver, events <-chan wm.Event, sink *history.Sink, src osproc.Source, forget func(int), turn *resolveTurn, debounce time.Duration) {
	// Go 1.23+ timer semantics: Stop/Reset never leave a stale value on the
	// channel, so the classic stop-and-drain dance is unnecessary. `pending`
	// tracks whether the timer is armed — the channel itself cannot answer that.
	timer := time.NewTimer(debounce)
	timer.Stop()
	defer timer.Stop()
	pending := false

	// flush runs a pending coalesce NOW and disarms the timer.
	flush := func() {
		if !pending {
			return
		}
		timer.Stop()
		pending = false
		reresolveAll(ctx, store, resolver, turn)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				// Stream closed (connection EOF or cancel). Flush rather than drop: the
				// burst's last state is exactly what a reconnect would otherwise have to
				// rediscover on its next tick.
				flush()
				return
			}
			if evt.Kind != wm.EventLayoutChanged {
				// Land the pending layout re-resolve BEFORE this event, restoring the
				// ordering the old `for evt := range events` loop had for free.
				//
				// It is load-bearing, not tidiness. Every wezterm window shares one mux
				// pid, so matchUniqueClient effectively joins on title alone and two
				// windows with equal titles leave BOTH sessions unresolved. The layout
				// event carrying a title change is precisely what disambiguates one of
				// them. Let a focus event overtake it and applyFocus finds no session
				// holding that address, unfocuses everything, and — since the focused id
				// changed — records a spurious focus event with an empty session id.
				// The real highlight then waits on the reconciler's backstop.
				//
				// Still strictly fewer re-resolves than before the debounce, which ran
				// one per layout event unconditionally.
				flush()
				handleWMEvent(ctx, store, resolver, evt, sink, src, forget, turn)
				continue
			}
			// Arm on the FIRST event of a burst rather than re-arming on every one.
			// Re-arming is a trailing-edge debounce, which has NO maximum wait: while
			// events keep arriving closer together than the window, the timer is reset
			// before it ever fires and the layout path re-resolves zero times. A
			// terminal that repaints its OS window title at animation rate during a
			// turn would do exactly that, and the staleness bound would silently become
			// the reconcile interval instead of the window. Arming once per burst gives
			// the "at most one re-resolve per window while events flow" this claims to
			// have; the burst's last state still lands, because an event after a firing
			// finds pending false and arms again.
			if !pending {
				timer.Reset(debounce)
				pending = true
			}
		case <-timer.C:
			pending = false
			reresolveAll(ctx, store, resolver, turn)
		}
	}
}

// handleWMEvent reacts to a neutral window event. Addresses arrive already
// normalized to Clients() form (the wm seam owns the Hyprland 0x quirk), so the
// daemon compares them directly against sess.Hyprland.Address.
func handleWMEvent(ctx context.Context, store *state.Store, resolver *mapping.Resolver, evt wm.Event, sink *history.Sink, src osproc.Source, forget func(int), turn *resolveTurn) {
	switch evt.Kind {
	case wm.EventWindowClosed:
		// A session's window went away — the "user closed the terminal while claude
		// was running" case. That is strong evidence the session is over (closing the
		// terminal SIGHUPs the shell), but it is NOT positive evidence the process
		// died, and this branch used to answer it with a bare delete: the session left
		// the store with no session_end written and no Forget called, putting it
		// permanently out of reach of the liveness sweep, which can only range the
		// store map. A fourth session-removal path bypassing the single-writer
		// invariant, and a ghost factory F1 structurally could not reach (L7).
		//
		// So apply the same two rules as everywhere else: close the lane through
		// endSession when the pid is definitively gone, and otherwise leave the
		// session tracked. A process not yet reaped is closed by the pidfd watch or by
		// the next sweep within one reconcile interval; a process that genuinely
		// outlived its window (detached, or a stale window mapping) keeps its lane,
		// which is what L4 demands — liveness is never inferred from a proxy signal.
		now := time.Now()
		store.Apply(func(m map[int]*state.Session) {
			for pid, sess := range m {
				if sess.Hyprland == nil || sess.Hyprland.Address != evt.Address {
					continue
				}
				if !sessionDead(src, pid) {
					continue
				}
				endSession(m, pid, sink, forget, now)
			}
		})
	case wm.EventFocusChanged:
		now := time.Now()
		store.Apply(func(m map[int]*state.Session) {
			applyFocus(m, evt.Address, sink, now)
		})
	case wm.EventLayoutChanged:
		// Something changed — kick a reconcile on any session that might match.
		//
		// The live stream never reaches this: drainWMEvents routes layout events to
		// its debounce instead, so in production this branch is unreachable. It is
		// kept correct rather than deleted because deleting it would make a future
		// caller that dispatches a layout event here silently update nothing, which
		// is a worse failure than the one it would prevent. It takes the same turn
		// as every other resolve, so reviving it cannot reintroduce the concurrent-
		// enumeration inversion — it would only bypass the debounce's rationing.
		reresolveAll(ctx, store, resolver, turn)
	}
}

// reresolveAll re-runs the terminal + WM match for every live session.
//
// This used to be commented "cheap: just iterate live sessions and re-resolve",
// which was the bug: resolver.Reconcile enumerates the terminal AND the WM once
// PER SESSION, so this is O(sessions) subprocess spawns — measured at 71% of the
// daemon's CPU — and it runs inside store.Apply, holding the lock every RPC
// reader and every hook queues behind. layoutDebounce exists to ration it.
func reresolveAll(ctx context.Context, store *state.Store, resolver *mapping.Resolver, turn *resolveTurn) {
	// Enumerate ONCE, outside the lock, exactly as reconcileOnce does. Hoisting
	// only the reconciler left this path still resolving per-session under the
	// lock, and a live 12-session box showed it immediately: the 5s spike train
	// vanished and was replaced by sub-second stalls up to 776ms, fired by the
	// layout events this path serves. The debounce above rations how often this
	// runs; this is what makes each run cheap.
	turn.Do(func() {
		panes, clients := resolver.Enumerate(ctx)
		now := time.Now()
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				resolveSession(ctx, resolver, sess, panes, clients, now)
			}
		})
	})
}

// resolveTurn serializes the two producers of the session→window mapping: the
// reconciler's tick and the WM layout path.
//
// Both now enumerate BEFORE taking the store lock. That is what made them fast,
// and it is also what introduced a write class that could not exist before: with
// sampling and writing no longer atomic, an enumeration taken at T0 can land
// AFTER one taken at T1 > T0, reverting a chip's workspace to the older reading
// until the next tick corrects it. Nothing blanks (ReconcileFrom no-ops on a
// miss) and nothing tears (each Apply writes one coherent enumeration), so the
// damage is bounded — but three separate behaviors were already leaning on "the
// next tick fixes it", and that is one too many.
//
// Holding this across enumerate-and-apply means the loser waits and then
// enumerates FRESH, so the last write always carries the freshest observation.
// It also stops the two paths from running duplicate concurrent enumerations,
// which is the very cost this change set exists to remove.
//
// ⚠ This is NOT the store lock and must never be conflated with it. No RPC
// reader, hook, or subscriber touches it, so a waiter here blocks nothing a user
// can feel — unlike the store lock, where a waiter is a chip click. Always take
// this BEFORE store.Apply, never the reverse.
type resolveTurn struct{ mu sync.Mutex }

// Do runs fn as the sole resolver. A nil turn runs fn unserialized, which keeps
// single-goroutine call sites (and tests) from having to construct one.
func (t *resolveTurn) Do(fn func()) {
	if t == nil {
		fn()
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fn()
}

// resolveSession applies a tick's already-fetched enumeration to one session,
// falling back to the per-session resolve — which does its own I/O — when the
// terminal backend offers no batch path.
//
// It exists so the reconciler and the WM layout path cannot drift apart on the
// rule that matters: the enumeration happens BEFORE store.Apply, never inside
// it. They drifted once already.
func resolveSession(ctx context.Context, resolver *mapping.Resolver, sess *state.Session, panes map[string]terminal.PaneRef, clients []wm.Window, now time.Time) {
	if panes != nil {
		resolver.ReconcileFrom(sess, panes, clients, now)
		return
	}
	resolver.Reconcile(ctx, sess)
}

// runReconciler periodically re-resolves every session's wezterm + hyprland
// mapping and re-syncs the Focused flag against the current active window.
// Catches anything missed by event-driven updates (e.g. a session whose
// mapping was incomplete when first created, the initial focus state, or a
// hyprctl race).
func runReconciler(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, interval time.Duration, tun statustune.Tuning, sink *history.Sink, obs *fanout.Observer, mem *memorySampler, forget func(int), turn *resolveTurn) {
	t := time.NewTicker(interval)
	defer t.Stop()
	rstate := newReconcileState(obs, mem)
	// The turn is taken around the WHOLE tick rather than threaded into
	// reconcileOnce, so the enumeration and the writes it feeds cannot be split by
	// the WM path landing an older observation between them. See resolveTurn.
	tick := func() {
		turn.Do(func() {
			reconcileOnce(ctx, store, resolver, manager, stack, tun, sink, rstate, forget)
		})
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

func reconcileOnce(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, tun statustune.Tuning, sink *history.Sink, rstate *reconcileState, forget func(int)) {
	// Re-publish capabilities every tick: the terminal locator is self-redetecting
	// (detect.NewAuto), so a terminal that came up after the daemon flips
	// terminal/navigate from their boot-race "none" values without a restart.
	store.SetCapabilities(stack.Capabilities())
	active, _ := manager.ActiveWindow(ctx)
	// The two enumerations the per-session resolve needs, fetched ONCE for the
	// whole tick and OUTSIDE the lock. This is the same rule the memory sample
	// below already follows, and it is why this function has this shape:
	// resolver.Reconcile does a terminal enumeration and a WM client query PER
	// SESSION, and it used to do them INSIDE store.Apply. A tick over N sessions
	// therefore held the write lock across N forks of `wezterm cli list` — 16 of
	// them on an 8-session, 2-mux box — and every RPC reader, every hook, and
	// every chip click queued behind it. Measured before this change: p99 166ms,
	// worst 1382ms, with the spike train landing exactly on the tick interval.
	panes, clients := resolver.Enumerate(ctx)
	// Stamped AFTER the enumeration so TitleAt still means "when the title was
	// sampled", which is what it meant when each session stamped its own clock on
	// the way out of Locate. Stamping before would backdate every title by the
	// fetch duration, and TitleAt is the freshness gate for the H9 idle-title
	// recovery (docs/timing-hazards.md).
	now := time.Now()
	// Memory is sampled BEFORE the lock is taken, against the pid set of the last
	// published snapshot: the reads are milliseconds and Store.Apply blocks every
	// RPC reader and every hook for as long as it holds. Only the assignment and
	// the sink.Record below run under the lock. See memorySampler.
	mem := rstate.sampleMemory(store)
	store.Apply(func(m map[int]*state.Session) {
		// Close the lanes of any session whose process is gone, BEFORE the per-tick
		// work below — a dead session earns none of it.
		sweepDeadSessions(m, stack.OSProc, sink, forget, now)
		for _, sess := range m {
			resolveSession(ctx, resolver, sess, panes, clients, now)
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
			// The session's resident cost, read outside this lock at the top of the
			// tick. The live fields take whatever the tick has, including a repeated
			// last-known figure after a failed read (better a stale tooltip than one
			// that flaps to zero); the log takes only a fresh reading, so a process
			// that is gone yields NO sample rather than a zero one — a zero would
			// read as "freed all its memory" and corrupt the peak and average.
			if reading, ok := mem.Sessions[sess.PID]; ok {
				sess.MemAgentBytes = reading.Agent.Pss
				sess.MemTreeBytes = reading.Tree.Pss
			}
			if ev, ok := mem.event(sess, now); ok {
				sink.Record(ev)
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
