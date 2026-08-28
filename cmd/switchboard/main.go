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

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/buildinfo"
	"github.com/tjmisko/switchboard/internal/detect"
	"github.com/tjmisko/switchboard/internal/discovery"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/projectname"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	codexprovider "github.com/tjmisko/switchboard/internal/provider/codex"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/transcript"
	"github.com/tjmisko/switchboard/internal/wm"
)

type codexObserverMode string

const (
	defaultCodexObserverMode codexObserverMode = "auto"
	codexObserverAuto        codexObserverMode = "auto"
	codexObserverOff         codexObserverMode = "off"
)

func parseCodexObserverMode(value string) (codexObserverMode, error) {
	switch mode := codexObserverMode(value); mode {
	case codexObserverAuto, codexObserverOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid Codex observer mode %q (want auto or off)", value)
	}
}

// codexObserverForMode keeps construction behind the rollout gate. In off
// mode the coordinator still projects Codex hooks; it simply has no app-server
// observer to register with, start, or request.
func codexObserverForMode(mode codexObserverMode, factory func() codexObserver) codexObserver {
	if mode == codexObserverOff {
		return nil
	}
	return factory()
}

func codexObserverModeCategory(mode codexObserverMode) string {
	if mode == codexObserverOff {
		return "observer_disabled"
	}
	return "observer_enabled"
}

func main() {
	var remoteFlags remoteDestinations
	statePath := flag.String("state", defaultStatePath(), "path to state.json mirror")
	socketPath := flag.String("socket", defaultSocketPath(), "path to RPC unix socket")
	scanInterval := flag.Duration("scan-interval", 1*time.Second, "/proc scan interval")
	reconcileInterval := flag.Duration("reconcile-interval", 5*time.Second, "full reconcile interval")
	wmFlag := flag.String("wm", "auto", "WM backend: auto|hyprland|sway|i3|x11|none")
	terminalFlag := flag.String("terminal", "auto", "terminal backend: auto|wezterm|tmux|none")
	historyDir := flag.String("history-dir", "", "activity-log directory (default $XDG_STATE_HOME/switchboard/history)")
	codexObserverFlag := flag.String("codex-observer", string(defaultCodexObserverMode), "Codex app-server observer: auto|off")
	codexDisplayNameModel := flag.String("codex-autoname-model", codexprovider.DefaultDisplayNameModel, "model for isolated Codex display-name generation")
	showVersion := flag.Bool("version", false, "print the build revision and exit")
	flag.Var(&remoteFlags, "remote", "SSH destination to observe (repeatable)")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Get())
		return
	}
	codexMode, err := parseCodexObserverMode(*codexObserverFlag)
	if err != nil {
		log.Fatal(err)
	}

	// The first line of every run names the build. A deploy that silently kept
	// the old binary is otherwise indistinguishable from one that worked: the
	// unit reports active either way. This line is what makes the journal a
	// record of which revision actually ran, and for how long.
	log.Printf("build: version=%s", buildinfo.Get())

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
	log.Printf("history: enabled=%t detail=%s retain_days=%d max_bytes=%s dir=%s",
		sink.Enabled(), histCfg.Detail, histCfg.RetainDays,
		history.HumanBytes(histCfg.MaxBytes), sink.Dir())

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
	dropStaleSessions(store, procSrc, sink, scanner.Forget, tun.TailBytes)
	resolver := mapping.NewResolver(term, manager)
	federated, err := newFederationRuntime(store, manager, remoteFlags)
	if err != nil {
		log.Fatalf("federation: %v", err)
	}
	// Both resolve paths enumerate WM clients through resolver.Enumerate. Hand
	// that list to federation instead of letting it fetch its own: it is the
	// list that says which local workspace displays each remote session, and so
	// where that session's chip belongs on this machine's bar. Installed before
	// the reconcile and WM loops start.
	resolver.SetWindowObserver(federated.ObserveWindows)

	// Provider observation is process-wide and graph-authoritative. Both periodic
	// and app-server/hook invalidations land through agentRuntime's generation
	// fence; observer I/O never runs under Store.Apply. The standalone Codex
	// app-server is a disposable read-only child and is closed exactly once with
	// the daemon.
	claudeObs := claudeprovider.NewObserver(sink.Dir(), claudeprovider.WithTuning(tun))
	codexObs := codexObserverForMode(codexMode, func() codexObserver {
		observer := codexprovider.NewObserver(codexprovider.Config{Diagnostic: func(category string) {
			// The adapter guarantees this callback contains only a finite, content-free
			// protocol category. Keep the log equally content-free.
			log.Printf("agent-observer: provider=codex category=%s count=1", category)
		}, WaitDiagnostic: func(diagnostic codexprovider.WaitClassificationDiagnostic) {
			log.Printf("agent-observer: provider=codex category=wait_episode event=%s episode=%d request_kind=%s ownership=%s evidence=%s source=%s duration_ms=%d red_duration_ms=%d red_published=%t human_evidence=%t cleared_without_human_evidence=%t suppressed_false_red=%t old_would_publish_red=%t count=1",
				diagnostic.Event, diagnostic.Episode, diagnostic.RequestKind, diagnostic.Ownership,
				diagnostic.Evidence, diagnostic.Source, diagnostic.Duration.Milliseconds(), diagnostic.RedDuration.Milliseconds(),
				diagnostic.RedPublished, diagnostic.HumanEvidence, diagnostic.ClearedWithoutHumanEvidence,
				diagnostic.SuppressedFalseRed, diagnostic.LegacyWouldPublishRed)
		}})
		log.Printf("agent-observer: provider=codex category=observer_constructed count=1")
		return observer
	})
	log.Printf("agent-observer: provider=codex category=%s count=1", codexObserverModeCategory(codexMode))
	agentRuntime := newAgentCoordinator(store, sink, claudeObs, codexObs)
	agentRuntime.SetCodexDisplayNamer(codexprovider.EphemeralNamer{}, *codexDisplayNameModel)
	agentRuntime.Start(ctx, *reconcileInterval)
	defer agentRuntime.Close()
	forgetRoot := func(pid int) {
		scanner.Forget(pid)
		// Forget/Close work is performed by the coordinator after this callback's
		// Store.Apply has released its lock.
		agentRuntime.RequestCleanup()
	}

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
		store.Apply(func(m map[int]*state.Session) {
			// A surviving hydrated root keeps its discovery-lifetime timestamp and
			// last-known provider projection while discovery refreshes only live
			// process/window fields. This is what lets restored graphs remain
			// authoritative until their explicit freshness deadline instead of being
			// erased on scan one. StartedAt is therefore not a kernel birth token.
			if prior := m[sess.PID]; prior != nil && prior.Agent == sess.Agent {
				sess.StartedAt = prior.StartedAt
				sess.DisplayName = prior.DisplayName
				sess.Claude, sess.Codex, sess.AgentGraph = prior.Claude, prior.Codex, prior.AgentGraph
			}
			m[sess.PID] = &sess
		})
		federated.AnnounceSession(ctx, sess)
		agentRuntime.Request(providerRootKey(sess))
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
				endSession(m, info.PID, sink, forgetRoot, time.Now())
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
	go runWMLoop(ctx, store, resolver, manager, sink, procSrc, forgetRoot, turn)
	go runReconciler(ctx, store, resolver, manager, stack, *reconcileInterval, tun, sink, nil, forgetRoot, turn)

	server := rpc.New(store, *socketPath, term, manager)
	server.SetTuning(tun)
	server.SetHistory(sink)
	server.SetAgentHookHandler(agentRuntime.HandleHook)
	server.SetAgentDiagnosticSource(agentRuntime.Diagnostics)
	server.SetHookAttributionDiagnostic(func(diagnostic rpc.HookAttributionDiagnostic) {
		agentRuntime.recordDiagnostic(agentgraph.ProviderCodex, diagnostic.Category, time.Now())
		if diagnostic.MatchedPID != 0 {
			log.Printf("codex-hook-attribution-probe: category=%s matched_pid=%d",
				diagnostic.Category, diagnostic.MatchedPID)
		}
	})
	federated.ConfigureServer(server)
	if err := os.MkdirAll(filepath.Dir(*socketPath), 0o755); err != nil {
		log.Fatalf("mkdir socket dir: %v", err)
	}
	if err := federated.StartViews(ctx); err != nil {
		cancel()
		federated.Wait()
		log.Fatalf("federation startup: %v", err)
	}
	ready := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ServeReady(ctx, ready) }()
	select {
	case err := <-serverErr:
		cancel()
		federated.Wait()
		if err != nil {
			log.Fatalf("rpc: %v", err)
		}
		return
	case <-ready:
	}
	log.Printf("switchboard listening on %s", *socketPath)
	federated.StartRemotes(ctx)
	err = <-serverErr
	cancel()
	federated.Wait()
	if err != nil {
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
//
// It is also where a surviving session's PROMPT OWNERSHIP is rebuilt (plan T12,
// §9). That belongs on these same lines and for the same stated reason: the
// startup instant re-stamped onto StatusSince is re-stamped onto every hydrated
// prompt, and the alternative — trusting the pre-restart clock — would make every
// pre-restart transcript entry read as "resolved after" and demote a red that was
// live across the restart. See hydratePending.
func dropStaleSessions(store *state.Store, procSrc osproc.Source, sink *history.Sink, forget func(int), tailBytes int64) {
	now := time.Now()
	// Sampled BEFORE the lock, per the direction the recent perf work established
	// (§9.4). Nothing is serving yet so there is no contention to create, but the
	// rule that transcript I/O stays outside store.Apply is worth keeping absolute.
	snapshot := store.Snapshot()
	verdicts := hydratePendingVerdicts(snapshot, tailBytes)
	type processVerdict struct {
		alive      bool
		definitive bool
	}
	processes := make(map[int]processVerdict, len(snapshot.Sessions))
	for _, sess := range snapshot.Sessions {
		info, err := procSrc.Read(sess.PID)
		processes[sess.PID] = processVerdict{
			alive:      err == nil && discovery.Classify(info) != discovery.AgentNone,
			definitive: errors.Is(err, osproc.ErrGone) || err == nil,
		}
	}
	store.Apply(func(m map[int]*state.Session) {
		for pid := range m {
			process, sampled := processes[pid]
			if !sampled {
				continue
			}
			if process.alive {
				// StatusSince is in-memory only (json:"-"), so it loads as zero. Stamp
				// it to startup time: the attention self-heal compares transcript
				// resolution times against it, and a zero value would read every old
				// tool_result as "resolved after" — wrongly demoting a still-pending
				// prompt that was live across the restart. Startup time keeps such a
				// chip red until something genuinely resolves after the restart.
				if info := m[pid].Enrichment(); info != nil {
					info.StatusSince = now
				}
				hydratePending(m[pid], verdicts[pid], now)
				continue
			}
			// Definitively dead (gone, or the pid recycled to a non-agent): record the
			// end that closes the lane. A non-definitive read error still drops the
			// stale entry, exactly as it always has, but must not fabricate an end for
			// a session that may well still be running.
			if process.definitive {
				endSession(m, pid, sink, forget, now)
			}
			delete(m, pid)
		}
	})
}

// hydratePendingVerdicts asks each hydrated session's transcripts whether the
// writers it persisted as blocked still have a tool in flight, returning
// pid → writer key → KEEP. It is pure I/O and pure reading: it runs before
// store.Apply, mutates nothing, and its verdicts can only ever remove ownership.
//
// Each writer is falsified against its OWN file — main against <session>.jsonl,
// a subagent against <session>/subagents/agent-<id>.jsonl (SubagentPath, from the
// stored transcript path, never re-derived from cwd). Crossing the two inverts the
// answer: live capture of subagent-raised prompts shows the MAIN tail fully
// matched throughout while the raising agent-*.jsonl carries the unmatched
// tool_use (§9.7, carry-over 2).
//
// Anything short of proof keeps the entry, so an unreadable file, a truncated
// tail, or a window that missed the tool_use all fail closed — the same reading
// permissionExit gives an unreadable transcript.
func hydratePendingVerdicts(snap state.Snapshot, tailBytes int64) map[int]map[string]bool {
	verdicts := map[int]map[string]bool{}
	for _, sess := range snap.Sessions {
		c := sess.Claude
		if c == nil || len(c.Pending) == 0 {
			continue
		}
		keep := make(map[string]bool, len(c.Pending))
		for _, writer := range c.PendingWriterKeys() {
			path := transcript.SubagentPath(c.Transcript, writer)
			evidence, err := transcript.BlockedByPendingTool(path, tailBytes)
			keep[writer] = evidence != transcript.BlockedNo
			if !keep[writer] {
				log.Printf("hydrate: pid=%d session=%s writer=%s resolved while the daemon was down (%s), dropping its prompt",
					sess.PID, shortSessionID(c.SessionID), pendingWriterLabel(writer), path)
			} else if err != nil {
				log.Printf("hydrate: pid=%d session=%s writer=%s transcript unreadable (%v), keeping its prompt",
					sess.PID, shortSessionID(c.SessionID), pendingWriterLabel(writer), err)
			}
		}
		verdicts[sess.PID] = keep
	}
	return verdicts
}

// hydratePending rebuilds one session's prompt ownership from what Load decoded,
// applying the verdicts sampled outside the lock. Claude-only: codex records no
// approvals in its rollout, so it has no ownership to restore and nothing that
// could ever resolve one.
//
// The three combinations §9.6 (trap 4) enumerates, all handled explicitly:
//
//	persisted status | pending_writers | action
//	permission       | non-empty       | keep the survivors, re-stamp Since := now
//	permission       | empty / absent  | a pre-T12 mirror: seed the main thread, which
//	                                     reproduces today's behavior exactly and is the
//	                                     honest downgrade across the version boundary
//	not permission   | non-empty       | unreachable by construction. Pending is the
//	                                     authority post-T5, so keep it, re-fold to RED,
//	                                     and log — a silent disagreement is how a
//	                                     missed RED hides
//
// Note the seed is keyed off whether writers were PERSISTED, not off whether the
// map is empty now: a set that the falsifier emptied has been proven resolved, and
// re-seeding it would manufacture the very red the falsifier just subtracted.
//
// Since is re-stamped rather than restored, deliberately. A true pre-restart onset
// makes every pre-restart transcript entry read as "resolved after," and running
// two clocks (true onset for T10's cap, restart for the resolution window) buys a
// marginal gain. The consequence, stated plainly: a prompt raised ten minutes
// before a restart gets a fresh full cap after it — the same #1-over-#2 trade the
// StatusSince re-stamp above already makes.
func hydratePending(sess *state.Session, keep map[string]bool, now time.Time) {
	c := sess.Claude
	if c == nil {
		return
	}
	persisted := len(c.Pending) > 0
	for _, writer := range c.PendingWriterKeys() {
		if !keep[writer] {
			c.DropPending(writer)
		}
	}
	switch {
	case c.Status == state.StatusPermission && !persisted:
		// Pre-T12 state.json: the red survived (status is on the wire and always
		// was) but its owner never did. Seeding the main thread restores exactly the
		// behavior this daemon had before the field existed — the red resolves
		// against the main transcript — instead of leaving an ownerless red that the
		// fold would read as not-red.
		c.SetPending("", state.PendingPrompt{Since: now})
		log.Printf("hydrate: pid=%d session=%s permission chip with no persisted owner; seeding the main thread (pre-T12 state.json)",
			sess.PID, shortSessionID(c.SessionID))
	case c.Status != state.StatusPermission && len(c.Pending) > 0:
		log.Printf("hydrate: pid=%d session=%s status=%q disagrees with %d persisted pending writer(s); Pending is the authority, re-folding to permission",
			sess.PID, shortSessionID(c.SessionID), c.Status, len(c.Pending))
		c.Status = state.StatusPermission
		c.StatusSince = now
	}
	for _, writer := range c.PendingWriterKeys() {
		p := c.Pending[writer]
		p.Since = now
		c.Pending[writer] = p
	}
}

// pendingWriterLabel names a Pending key for a log line, since the main thread's
// key is the empty string and an empty writer= reads as a bug.
func pendingWriterLabel(writer string) string {
	if writer == "" {
		return state.PendingWriterMain
	}
	return writer
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
		type deadWindowRoot struct {
			pid       int
			startedAt time.Time
		}
		var dead []deadWindowRoot
		for _, sess := range store.Snapshot().Sessions {
			if sess.Hyprland == nil || sess.Hyprland.Address != evt.Address || !sessionDead(src, sess.PID) {
				continue
			}
			dead = append(dead, deadWindowRoot{pid: sess.PID, startedAt: sess.StartedAt})
		}
		store.Apply(func(m map[int]*state.Session) {
			for _, root := range dead {
				sess := m[root.pid]
				if sess == nil || !sess.StartedAt.Equal(root.startedAt) || sess.Hyprland == nil || sess.Hyprland.Address != evt.Address {
					continue
				}
				endSession(m, root.pid, sink, forget, now)
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
		snapshot := store.Snapshot()
		resolved := make(map[int]state.Session, len(snapshot.Sessions))
		for _, sess := range snapshot.Sessions {
			copy := cloneSessionForReconcile(sess)
			resolveSession(ctx, resolver, &copy, panes, clients, now)
			resolved[sess.PID] = copy
		}
		store.Apply(func(m map[int]*state.Session) {
			for pid, value := range resolved {
				sess := m[pid]
				if sess == nil || !sess.StartedAt.Equal(value.StartedAt) {
					continue
				}
				sess.Wezterm = value.Wezterm
				sess.Hyprland = value.Hyprland
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
func runReconciler(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, interval time.Duration, tun statustune.Tuning, sink *history.Sink, obs *fanout.Observer, forget func(int), turn *resolveTurn) {
	t := time.NewTicker(interval)
	defer t.Stop()
	rstate := newReconcileState(obs)
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
	// whole tick and OUTSIDE the lock. It is why this function has this shape:
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

	// Prepare every per-session process/provider decision from a detached
	// snapshot. This is the lock boundary for the reconcile path: /proc liveness
	// and suspension, terminal fallback resolution, transcript status recovery,
	// fanout, label, and usage reads all finish before Store.Apply begins.
	type preparedSession struct {
		before    state.Session
		after     state.Session
		dead      bool
		suspended *bool
	}
	snapshot := store.Snapshot()
	prepared := make(map[int]preparedSession, len(snapshot.Sessions))
	legacy := make(map[int]*state.Session)
	for i := range snapshot.Sessions {
		before := snapshot.Sessions[i]
		item := preparedSession{before: before, after: cloneSessionForReconcile(before)}
		item.dead = sessionDead(stack.OSProc, before.PID)
		if !item.dead {
			resolveSession(ctx, resolver, &item.after, panes, clients, now)
			if processState, err := proc.State(before.PID); err == nil {
				suspended := proc.Suspended(processState)
				item.suspended = &suspended
			}
			if item.after.Claude != nil {
				if item.after.AgentGraph == nil {
					rstate.observe(sink, &item.after, item.after.Claude, now)
					legacy[item.after.PID] = &item.after
				} else {
					rstate.observeAuxiliary(sink, &item.after, item.after.Claude, now)
				}
			}
		}
		prepared[before.PID] = item
	}
	// The legacy recovery remains only for sessions that have not entered graph
	// authority. Its bounded transcript reads now run against detached values.
	selfHealStaleAttention(legacy, now, tun, sink)
	selfHealStuckStatus(legacy, now, tun, sink)
	for pid, sess := range legacy {
		item := prepared[pid]
		item.after = *sess
		prepared[pid] = item
	}

	store.Apply(func(m map[int]*state.Session) {
		for pid, item := range prepared {
			sess := m[pid]
			if sess == nil || !sess.StartedAt.Equal(item.before.StartedAt) {
				continue
			}
			if item.dead {
				if endSession(m, pid, sink, forget, now) {
					log.Printf("liveness sweep: pid=%d gone, closed its lane", pid)
				}
				continue
			}
			// Only detached, already-resolved values are copied under the lock.
			sess.Wezterm = item.after.Wezterm
			sess.Hyprland = item.after.Hyprland
			// Refresh job-control suspension (Ctrl-Z). On ErrGone the sweep above has
			// already dropped the session, so this only ever sees a live pid; leave
			// the last-known value on any other read error rather than flapping. A
			// change is logged to history as a suspend/resume edge (it greys/un-greys
			// the chip in a timeline).
			if item.suspended != nil {
				susp := *item.suspended
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
			// A graph that landed after the detached preparation wins. Otherwise copy
			// the legacy adapter-less projection only if its session identity did not
			// rotate while the reads were in flight.
			if item.after.Claude != nil && sess.AgentGraph == nil &&
				(sess.Claude == nil || sess.Claude.SessionID == item.before.Claude.SessionID) {
				sess.Claude = item.after.Claude
			}
		}
		// Re-sync focus against the active window (the backstop for any focus event
		// the live socket2 stream missed) and record a focus edge on a real change.
		// Runs after the resolve loop so every session's Hyprland address is current.
		applyFocus(m, active, sink, now)
		rstate.prune(m)
	})
}

// cloneSessionForReconcile detaches the mutable pointers touched by terminal
// resolution and legacy Claude recovery. AgentGraph is immutable at this
// boundary and is never mutated by the legacy path.
func cloneSessionForReconcile(sess state.Session) state.Session {
	clone := sess
	if sess.Wezterm != nil {
		value := *sess.Wezterm
		clone.Wezterm = &value
	}
	if sess.Hyprland != nil {
		value := *sess.Hyprland
		clone.Hyprland = &value
	}
	if sess.Claude != nil {
		value := *sess.Claude
		value.Workflows = append([]state.WorkflowStatus(nil), sess.Claude.Workflows...)
		value.PendingWriters = append([]string(nil), sess.Claude.PendingWriters...)
		if sess.Claude.Pending != nil {
			value.Pending = make(map[string]state.PendingPrompt, len(sess.Claude.Pending))
			for writer, prompt := range sess.Claude.Pending {
				value.Pending[writer] = prompt
			}
		}
		clone.Claude = &value
	}
	return clone
}

// recordReconcileTransition mirrors a hookless reconciler status edge into the
// activity log, computing the closed interval's length from the still-current
// StatusSince (call it BEFORE re-stamping StatusSince). A no-op on a disabled sink.
//
// pendingTool is passed rather than read off c because the permission exit removes
// the prompts BEFORE it records the edge (it cannot know the chip is leaving red
// until every entry is gone), and an exit edge whose `pending` reads empty loses
// the one thing that edge exists to explain: which tool the released red was for.
// The history event's `pending` is a TOOL name (docs/state-schema.md), so it takes
// the derived scalar rather than the decision log's multi-writer summary — a
// structured field must not start carrying a "+N" suffix.
func recordReconcileTransition(sink *history.Sink, sess *state.Session, c *state.AgentInfo, to, rule, reason, pendingTool string, now time.Time) {
	sink.Record(history.Event{
		Ts: now, Type: history.EventTransition,
		SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		From: c.Status, To: to, Rule: rule, Reason: reason,
		Subagents: c.InFlightSubagents, Pending: pendingTool,
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
// state has nothing to release it. For each blocked WRITER it reads the tail of
// that writer's transcript and asks whether the writer's own thread advanced past
// its prompt: an assistant message or a user interrupt notice means it was
// answered/declined → drop that writer's entry; otherwise it is still pending →
// stay red. Crucially, a bare tool_result is not treated as resolution — a
// background teammate/subagent or a sibling auto-tool keeps writing tool_results
// while the prompt waits, and counting them would flash the chip green the instant
// any concurrent work landed. A pending decision stays red even while subagents
// work.
//
// # T9 — resolution is routed to the writer that raised the prompt
//
// Each Pending[a] is resolved against transcript.SubagentPath(c.Transcript, a),
// which returns the main transcript unchanged for a == "". Asking the MAIN file
// about every prompt (what this did before) is defect 4 of the 2026-08-05 incident
// (docs/subagent-permission-oscillation.md §3.5): ResolveKind reports
// ResolutionResumed for ANY assistant message dated after the prompt, so a main
// thread that merely keeps working while a teammate is blocked emits a stream of
// messages that read as "the prompt resolved." That is the single reason a red
// could not survive a working main thread. Main-thread activity is not evidence
// about a subagent's prompt, and a teammate's is not evidence about the main
// thread's — the measured shape of a blocked writer is the mirror image: it goes
// quiescent within ~1s of its PermissionRequest and stays quiescent while the
// other writers keep advancing (§4.3).
//
// Removal is therefore per writer (DropPending, never ClearPending), and the chip
// leaves "permission" only once the map is empty (plan §3.3) — case 18: with the
// main thread and a teammate both blocked, answering one keeps the chip red.
//
// # T10 — the per-prompt liveness backstop
//
// A writer that is gone, or whose own file has been quiescent past
// PendingWriterStaleCap, has its entry dropped as unanswerable (case 19). Without
// it the generalized hold (T3) lets a crashed teammate's prompt latch red forever
// — plan risk R3, which T3 widened. See writerQuiescentPastCap.
//
// It runs against detached session copies prepared before reconcile Apply. Only
// the already-decided projection is assigned under the store lock; the bounded
// transcript reads therefore never delay RPC readers or graph-hook delivery.
func selfHealStaleAttention(m map[int]*state.Session, now time.Time, tun statustune.Tuning, sink *history.Sink) {
	for _, sess := range m {
		c := sess.Claude
		if c == nil || sess.AgentGraph != nil || c.Status != state.StatusPermission {
			continue
		}
		age := now.Sub(c.StatusSince)
		// Snapshot before any drop so the exit line and the exit history event report
		// the state the decision was made against, exactly as the pre-T9 pair did.
		summary, pendingTool := c.PendingSummary(), c.PendingTool

		// Two passes, so the outcome does not depend on which writer happened to be
		// visited first: decide every writer against the map as the tick found it,
		// then remove them together.
		var resolved []writerVerdict
		for _, writer := range pendingWriters(c) {
			v, ok := resolveWriterPrompt(c, writer, now, tun)
			if !ok {
				continue // this writer is still blocked → its entry, and the red, stay
			}
			resolved = append(resolved, v)
		}
		if len(resolved) == 0 {
			continue // nothing resolved → keep red, silently
		}
		for _, v := range resolved {
			c.DropPending(v.writer)
		}
		if len(c.Pending) > 0 {
			// Case 18: a real answer landed, and the chip still must not change color.
			// Logged as a hold — silence here is exactly how "I approved it and it is
			// still red" becomes unanswerable from the journal.
			statustune.Decision{
				PID: sess.PID, Session: shortSessionID(c.SessionID),
				From: state.StatusPermission, To: state.StatusPermission,
				Rule:      statustune.RuleHoldOtherWriters,
				Reason:    fmt.Sprintf("%s resolved; %d writer(s) still blocked", verdictWriters(resolved), len(c.Pending)),
				Subagents: c.InFlightSubagents, Pending: c.PendingSummary(), Age: age,
			}.Log()
			continue
		}
		// The map is empty, so the red is genuinely over. The exit color comes from
		// the LAST prompt's resolution kind (plan §3.3, the existing P3 rule).
		last := resolved[len(resolved)-1]
		// This transition has no Claude Code hook behind it (a declined or
		// interrupted prompt fires none), so unlike the hook-driven edges it would
		// otherwise leave no trace. The decision log records WHICH rule fired and
		// the full observed state, so a self-healed red chip — and its exit color —
		// is fully reconstructable from the journal.
		statustune.Decision{
			PID: sess.PID, Session: shortSessionID(c.SessionID),
			From: state.StatusPermission, To: last.exit, Rule: last.rule, Reason: last.reason,
			Subagents: c.InFlightSubagents, Pending: summary, Age: age,
		}.Log()
		recordReconcileTransition(sink, sess, c, last.exit, last.rule, last.reason, pendingTool, now)
		c.Status = last.exit
		c.StatusSince = now
	}
}

// writerVerdict is one blocked writer's resolution: which writer, and the exit
// color/rule/reason its evidence selected.
type writerVerdict struct {
	writer string
	exit   string
	rule   string
	reason string
}

// verdictWriters names the resolved writers for a log line, in the deterministic
// order they were decided.
func verdictWriters(vs []writerVerdict) string {
	names := make([]string, 0, len(vs))
	for _, v := range vs {
		names = append(names, pendingWriterLabel(v.writer))
	}
	return strings.Join(names, ",")
}

// pendingWriters is the writer set selfHealStaleAttention resolves for one red
// chip: Pending's keys in their deterministic order (PendingWriterKeys — never a
// bare map range, whose order Go randomizes), or the MAIN THREAD alone when the
// chip is red with nothing recorded against it.
//
// The fallback is the same reading hydratePending gives an ownerless red: red is
// owned by Pending post-T5, so an entry-less permission chip is a pre-T5 artifact
// (a hand-seeded status, a mirror written by an older daemon), and resolving it
// against the main transcript reproduces exactly the behavior this daemon had
// before the map existed. Skipping it instead would strand such a chip red with
// nothing able to release it.
func pendingWriters(c *state.AgentInfo) []string {
	if keys := c.PendingWriterKeys(); len(keys) > 0 {
		return keys
	}
	return []string{""}
}

// resolveWriterPrompt decides one blocked writer's prompt against that writer's
// OWN transcript, returning ok=false to keep it (and the red).
//
// The prompt's own Since dates the read, not the chip's StatusSince: with two
// writers blocked the chip's stamp belongs to whichever went red first, and a
// prompt raised later must not be resolved by entries that predate it. It falls
// back to StatusSince for an entry that carries no onset — a hand-seeded chip, or
// the ownerless main writer pendingWriters synthesizes.
//
// The unreadable branch (case 15's TTL backstop) is deliberately restricted to the
// MAIN writer. c.Transcript is the file the daemon derives every other signal
// from, so its unreadability is a session-level fault and case 15 is the
// session-level fail-soft. A missing agent-<id>.jsonl means nothing of the sort:
// it is the normal state for a just-spawned teammate and for any id the mapping
// cannot resolve, and SubagentPath's contract is explicit that a failed read must
// leave the caller's entry in place (an under-stripped id derives a path that does
// not exist, and fail-safe is the direction to err in). So a subagent's unreadable
// file falls through to ResolutionNone — keep — and is bounded by T10's cap
// instead, which is 60× longer than the TTL.
func resolveWriterPrompt(c *state.AgentInfo, writer string, now time.Time, tun statustune.Tuning) (writerVerdict, bool) {
	since := c.Pending[writer].Since
	if since.IsZero() {
		since = c.StatusSince
	}
	path := transcript.SubagentPath(c.Transcript, writer)
	kind, err := transcript.ResolveKind(path, since, tun.TailBytes)
	exit, rule, reason, ok := permissionExit(kind, err != nil && writer == "", now.Sub(since), c.InFlightSubagents, tun)
	if ok {
		return writerVerdict{writer, exit, rule, reason}, true
	}
	// Before the liveness backstop can fire, ask whether this writer is still
	// DEMONSTRABLY blocked. A blocked writer stops writing because it is blocked,
	// so quiescence alone cannot tell a live prompt from a dead one — but the tail
	// can, and hydratePendingVerdicts already asks it this exact question. Proof
	// that the tool is still unanswered outranks any clock.
	if evidence, err := transcript.BlockedByPendingTool(path, tun.TailBytes); err == nil && evidence == transcript.BlockedYes {
		return writerVerdict{}, false
	}
	if !writerQuiescentPastCap(path, since, now, tun.PendingWriterStaleCap) {
		return writerVerdict{}, false
	}
	return writerVerdict{
		writer: writer,
		exit:   tun.InterruptExitStatus,
		rule:   statustune.RuleStaleWriterBackstop,
		reason: fmt.Sprintf("%s quiescent past the %s cap", pendingWriterLabel(writer), tun.PendingWriterStaleCap),
	}, true
}

// writerQuiescentPastCap is T10's per-prompt liveness backstop (case 19): it
// reports whether writer `path`'s prompt has become unanswerable, either because
// the writer is gone or because its own file has stopped moving for longer than
// the cap.
//
// The policy is the fanout Observer's, not a second one invented here. That
// Observer force-closes a spawned-but-unfinished subagent as completion=unknown
// once its jsonl mtime is older than fanout.DefaultStaleCap, so in-flight cannot
// leak (internal/fanout/observer.go). This is the same measurement applied to the
// same file for the same reason: at the default cap the two agree, and the chip
// never stays red for a teammate the Observer has already stopped counting.
//
// The clock runs from the LATER of the prompt's onset and the file's mtime:
//
//   - mtime later — an active writer resets it on every write, so only a genuinely
//     stalled one accumulates age. A blocked writer stops within ~1s of its
//     PermissionRequest, so in practice its clock starts at the prompt.
//   - onset later — a prompt raised against a file that was already old (the
//     hydrate path re-stamps Since to startup) gets a fresh FULL cap rather than
//     inheriting the stale file's age. §9.6 states that trade explicitly.
//
// A file that cannot be stat-ed contributes nothing, so the clock runs from the
// onset alone: "the writer is gone" and "the writer went quiet" are the same
// verdict here, reached the same way, which is what keeps a prompt from a crashed
// teammate from latching red forever.
//
// Stated plainly, because it is a real cost: a genuinely pending prompt is
// indistinguishable from a dead one BY THIS MEASUREMENT — both are a quiescent
// file — so a user who walks away for longer than the cap comes back to a chip
// that stopped nagging. That is why the cap sits at 30 minutes rather than
// anywhere near a plausible time-to-answer, and why it is a Tuning field.
//
// By this measurement, but not by every measurement — and relying on this one
// alone was a missed RED. A writer blocked overnight on a Bash approval had its
// red dropped 47 minutes in (2026-08-14, session 6016b3e9), because a blocked
// writer goes quiet precisely BY BEING blocked. The tail distinguishes the two
// cases even though the mtime cannot: a live prompt leaves an unanswered
// tool_use sitting at the end of the file, while an abandoned writer's last
// exchange is complete. Measured over every subagent transcript on the
// development machine (362): 311 ended at end_turn, 50 were abandoned on a
// COMPLETED exchange, and exactly 1 held a dangling tool_use — 550 hours old,
// in a session long dead. So resolveWriterPrompt consults
// transcript.BlockedByPendingTool first and this backstop now only judges
// writers with no such proof.
//
// That leaves the crashed-mid-tool-use case (R3) held red indefinitely, which
// is deliberate and is bounded elsewhere: a subagent owns no pid to probe, but
// its parent does, and a session whose process is gone leaves the store
// entirely and takes its chip with it.
//
// A zero onset (a chip with no Since at all) and a non-positive cap both disable
// the backstop — the same shape as the Observer's zero-ModTime guard, and the
// reason a mis-stamped chip fails toward red rather than away from it.
func writerQuiescentPastCap(path string, since, now time.Time, staleCap time.Duration) bool {
	if staleCap <= 0 || since.IsZero() {
		return false
	}
	quiescentSince := since
	if fi, err := os.Stat(path); err == nil && fi.ModTime().After(quiescentSince) {
		quiescentSince = fi.ModTime()
	}
	return now.Sub(quiescentSince) >= staleCap
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
// detached pre-Apply reconcile pass, exactly like selfHealStaleAttention. Every flip re-stamps
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
		if c == nil || sess.AgentGraph != nil {
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
		Subagents: c.InFlightSubagents, Pending: c.PendingSummary(),
		Age: now.Sub(c.StatusSince),
	}.Log()
	recordReconcileTransition(sink, sess, c, to, rule, reason, c.PendingTool, now)
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
