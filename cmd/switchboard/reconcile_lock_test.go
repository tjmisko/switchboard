package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/detect"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
)

// enumDelay stands in for the real cost of enumerating a terminal: on the live
// box a tick forked 16 `wezterm cli list` calls and took 96-129ms even when the
// machine was quiet. Exaggerated here so the assertion has room that a loaded
// CI scheduler cannot erase.
const enumDelay = 300 * time.Millisecond

// slowBatchLocator is a terminal backend whose enumeration is expensive, on both
// the batch and the per-session path. It counts each so a test can pin how many
// enumerations one reconcile tick costs.
type slowBatchLocator struct {
	mu        sync.Mutex
	snapshots int
	locates   int
	panes     map[string]terminal.PaneRef
}

func (l *slowBatchLocator) Name() string    { return "slowbatch" }
func (l *slowBatchLocator) Available() bool { return true }

func (l *slowBatchLocator) Snapshot(context.Context) (map[string]terminal.PaneRef, error) {
	l.mu.Lock()
	l.snapshots++
	l.mu.Unlock()
	time.Sleep(enumDelay)
	return l.panes, nil
}

func (l *slowBatchLocator) Locate(_ context.Context, tty string) (*terminal.PaneRef, error) {
	l.mu.Lock()
	l.locates++
	l.mu.Unlock()
	time.Sleep(enumDelay)
	if ref, ok := l.panes[tty]; ok {
		return &ref, nil
	}
	return nil, nil
}

func (l *slowBatchLocator) Activate(context.Context, *terminal.PaneRef) error { return nil }

func (l *slowBatchLocator) counts() (snapshots, locates int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshots, l.locates
}

// fixtureConfig is what the options below assemble. The zero value is the
// original fixture: Claude enrichment left nil, so the tick does no transcript
// I/O and the fixture is about the resolve alone.
type fixtureConfig struct {
	seed *slowSeed // non-nil once withSlowFanoutSeed is applied
}

type fixtureOption func(*testing.T, *fixtureConfig)

// reconcileFixture builds a reconcileOnce call site over fakes, with sessionCount
// sessions already tracked.
func reconcileFixture(t *testing.T, sessionCount int, opts ...fixtureOption) (*state.Store, *slowBatchLocator, *mapping.Resolver, func()) {
	t.Helper()

	var cfg fixtureConfig
	for _, opt := range opts {
		opt(t, &cfg)
	}

	panes := make(map[string]terminal.PaneRef, sessionCount)
	store := state.New("")
	procs := map[int]procState{}
	store.Apply(func(m map[int]*state.Session) {
		for i := range sessionCount {
			pid := 5000 + i
			tty := ttyName(i)
			procs[pid] = procAlive
			sess := &state.Session{PID: pid, TTY: tty, CWD: "/home/test", StartedAt: time.Now()}
			if cfg.seed != nil {
				// Never seen by the Observer, which is what makes the FIRST tick pay
				// the first-sight seed — the cost B2 budgets.
				sess.Agent = state.AgentKindClaude
				sess.Claude = &state.AgentInfo{
					SessionID:  cfg.seed.sessionID(i),
					Transcript: cfg.seed.transcript(t, i),
				}
			}
			m[pid] = sess
			panes[tty] = terminal.PaneRef{Backend: "slowbatch", TTY: tty, PaneID: i}
		}
	})

	loc := &slowBatchLocator{panes: panes}
	manager := stubManager{}
	stack := detect.Stack{OSProc: fakeProcSource{st: procs}, Terminal: loc, WM: manager}
	resolver := mapping.NewResolver(loc, manager)
	historyDir := t.TempDir()
	if cfg.seed != nil {
		historyDir = cfg.seed.dir
	}
	rstate := newReconcileState(fanout.NewObserver(historyDir), nil)

	tick := func() {
		reconcileOnce(context.Background(), store, resolver, manager, stack,
			statustune.Default(), nil, rstate, func(int) {}, nil)
	}
	return store, loc, resolver, tick
}

// measureWorstReaderWait runs work while a goroutine hammers store.Snapshot, and
// reports the longest a reader was blocked. Those readers stand in for what
// actually queues behind this lock in production: every waybar subscriber, every
// hook RPC from a live session, and `switchboard-ctl focus` — the chip click.
//
// ⚠ It answers ONE question now, and it is not "how long was the lock held".
// After publish-and-swap Snapshot() is an atomic pointer load, so this reports ~0
// for every workload BY CONSTRUCTION and any budget written against it passes
// vacuously. Its one live use is TestShouldNotBlockAReaderWhileAWriterHoldsTheLock,
// where ~0 IS the assertion. To budget a lock hold, use measureWorstWriterWait.
func measureWorstReaderWait(store *state.Store, work func()) time.Duration {
	stop := make(chan struct{})
	var mu sync.Mutex
	worst := time.Duration(0)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			store.Snapshot()
			waited := time.Since(start)
			mu.Lock()
			if waited > worst {
				worst = waited
			}
			mu.Unlock()
		}
	}()

	work()
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	return worst
}

// writerProbeInterval paces the competing writer in measureWorstWriterWait.
const writerProbeInterval = 250 * time.Microsecond

// measureWorstWriterWait runs work while a goroutine hammers store.Apply with an
// empty mutation, and reports the longest one of those writes waited to get in.
//
// It is measureWorstReaderWait's twin, and after publish-and-swap it is the twin
// with teeth. Readers now load a published pointer and never take the lock at all,
// so the reader-side number collapses to ~0 BY CONSTRUCTION — a reader-wait budget
// would pass no matter what the tick did under the lock, which is the exact shape
// of the vacuous test this file already had to fix once (see the note on
// locateOnlyLocator.Locate). Writers still queue, so the longest a competing write
// waited is the tick's Apply hold, measured from outside.
//
// The ideal instrument is the hold itself, which state.Apply already times and
// state.lockWarnOut already exposes "purely so the test can read it". Both are
// unexported and this test lives one package out, so it asks the question in the
// terms available here. The quantities differ by the microseconds of an empty
// Apply, against a budget in the tens of milliseconds.
func measureWorstWriterWait(store *state.Store, work func()) time.Duration {
	stop := make(chan struct{})
	var mu sync.Mutex
	worst := time.Duration(0)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// An empty mutation changes nothing, so this neither broadcasts nor
			// persists — it only competes for the lock, which is the measurement.
			start := time.Now()
			store.Apply(func(map[int]*state.Session) {})
			waited := time.Since(start)
			mu.Lock()
			if waited > worst {
				worst = waited
			}
			mu.Unlock()
			// Paced, not spun, and the pacing is what makes the number readable.
			// Every Apply builds a full snapshot and encodes it for the change key,
			// so an unthrottled loop allocates hard enough that its own GC assists
			// dominate the measurement — an idle store read as 7 ms of "wait" before
			// this sleep existed, against budgets in the tens of milliseconds. The
			// interval is far shorter than any hold worth failing on, so nothing the
			// budget cares about can slip between two attempts.
			time.Sleep(writerProbeInterval)
		}
	}()

	work()
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	return worst
}

func ttyName(i int) string {
	return "/dev/pts/" + string(rune('0'+i))
}

// ---------------------------------------------------------------------------
// The tick budget: an injected slow seed, and a bound on what the Apply may cost
// ---------------------------------------------------------------------------

// These size the fixture's history archive. The target is a first-sight seed in
// the tens of milliseconds — big enough that a budget has room a loaded scheduler
// cannot erase, small enough to write in a unit test. The live archive behind the
// 1.81 s figure this whole change set exists for was 62.6 MB over 37 days; this is
// a scale model of it, ~28 MB over 10.
//
// seedArchiveMatchEvery is why it is that shape rather than a tenth the size with
// every line matching. The seed's cost has two parts — a byte pre-filter over
// EVERY line and a JSON decode of the few that pass it — and only the first part
// is free of garbage. An all-matching archive reaches the same duration in a fifth
// the bytes and allocates 40 MB doing it, and that garbage lands as multi-
// millisecond GC pauses in the very measurement being taken: the budget's own
// noise floor went from ~0.3 ms to ~16 ms, against a limit of 20. So the archive
// is mostly OTHER sessions' events, which is also what a real day-file is —
// dominated by everyone else's transitions and usage samples, and exactly the
// shape the pre-filter was written for.
//
// The matching lines carry BOTH the quoted session id and the "workflow_"
// substring. seedFor calls PriorSubagentState and PriorWorkflowState, each
// pre-filtering on its own needle pair, so an archive satisfying only one needle
// exercises only half the seed — which is precisely the half-measure that made an
// earlier reading of this cost understate it by 17x.
// seedArchiveSessions is how many sessions the budget test runs, and how many
// distinct ids the matching lines are spread across, so every session's seed
// decodes an equal share and the one measured probe speaks for all of them. It is
// deliberately SMALL where the sibling enumeration tests use eight: each session
// re-reads the whole archive on first sight, and that I/O is the measurement's own
// noise floor. Three sessions is a real tick and keeps the floor under a
// millisecond against a budget of tens.
const (
	seedArchiveDays       = 10
	seedArchiveLines      = 16000
	seedArchiveMatchEvery = 500
	seedArchiveSessions   = 3
)

// minSeedCost is the vacuity floor. A budget expressed as a fraction of an
// injected delay proves nothing if the delay stopped being injected — a faster
// decoder, a smaller fixture, a machine with a much faster disk — so the test
// fails loudly rather than passing on a seed that costs nothing.
const minSeedCost = 25 * time.Millisecond

// slowSeed is the injected delay: a history archive big enough that one session's
// first-sight seed costs real time, plus the measured cost of paying it once.
//
// The cost is MEASURED rather than assumed because the seed's price is the
// decoder's, not a sleep's. Budgeting against a wall-clock constant would fail on
// a loaded CI box and pass on a fast one; budgeting against what this box actually
// paid, on this fixture, moments ago, scales with the box.
type slowSeed struct {
	dir  string
	base string
	cost time.Duration
}

func (s *slowSeed) sessionID(i int) string { return fmt.Sprintf("s-seed-%d", i) }

// transcript writes session i's transcript and returns its path. It exists so the
// pre-lock sample's transcript reads succeed; a sample that failed to read would
// be rejected and re-read inline, which would put I/O back under the lock for
// reasons having nothing to do with the seed.
func (s *slowSeed) transcript(t *testing.T, i int) string {
	t.Helper()
	path := filepath.Join(s.base, s.sessionID(i)+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"system"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// withSlowFanoutSeed gives the fixture's sessions Claude enrichment no Observer
// has ever seen, backs that Observer with a slow history archive, and records
// what one seed against it costs.
//
// This is the gap B2 closes. The fixture made only the TERMINAL ENUMERATION slow,
// so it pinned the steady-state body of the lock hold and said nothing about its
// multi-second tail — and the tail is what a newly-seen session pays. The live
// 30-minute window meant to measure that never saw a session appear, so the seed
// was never exercised at all. This is the deterministic version of the experiment
// that window failed to run.
func withSlowFanoutSeed(seed *slowSeed) fixtureOption {
	return func(t *testing.T, cfg *fixtureConfig) {
		t.Helper()
		seed.dir, seed.base = t.TempDir(), t.TempDir()

		for day := range seedArchiveDays {
			var b strings.Builder
			for i := range seedArchiveLines {
				// Most lines belong to sessions this seed is not asking about and are
				// dropped by the pre-filter without being decoded; every
				// seedArchiveMatchEvery-th belongs to one of the fixture's own.
				sid := fmt.Sprintf("s-bystander-%d", i)
				if i%seedArchiveMatchEvery == 0 {
					sid = seed.sessionID((i / seedArchiveMatchEvery) % seedArchiveSessions)
				}
				fmt.Fprintf(&b, `{"ts":"2026-06-%02dT12:00:00Z","type":"subagent_spawn","session_id":%q,`+
					`"agent_id":"a%d","workflow_run_id":"wf_%d","pid":4242,"agent":"claude","cwd":"/home/test"}`+"\n",
					day+1, sid, i, i)
			}
			path := filepath.Join(seed.dir, fmt.Sprintf("2026-06-%02d.jsonl", day+1))
			if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		// Time one seed on a throwaway Observer. The first call is discarded so the
		// figure and the tick that follows meet the same warm page cache — a cold
		// reading would over-budget the tick and let a real regression through.
		probe := seed.transcript(t, 0)
		fanout.NewObserver(seed.dir).Prime(seed.sessionID(0), probe)
		start := time.Now()
		fanout.NewObserver(seed.dir).Prime(seed.sessionID(0), probe)
		seed.cost = time.Since(start)

		cfg.seed = seed
	}
}

// TestShouldNotHoldTheStoreLockAcrossTheFanoutSeed is the test that would have
// caught #61 directly.
//
// The seed is the most expensive thing the tick does and it is paid per NEWLY-SEEN
// session — not once at startup, as the pre-measurement framing assumed, but every
// time a session appears on a box whose whole purpose is watching sessions come
// and go. Production put it at 1.57-2.36 s per seed with the exclusive lock held.
//
// It budgets the WRITER side deliberately; see measureWorstWriterWait for why the
// reader side went vacuous the moment readers stopped taking the lock. Post
// publish-and-swap the assertion reads "the tick's Apply is short", which is still
// exactly the invariant: the seed belongs in Prime, before the lock.
func TestShouldNotHoldTheStoreLockAcrossTheFanoutSeed(t *testing.T) {
	var seed slowSeed
	store, _, _, tick := reconcileFixture(t, seedArchiveSessions, withSlowFanoutSeed(&seed))

	if seed.cost < minSeedCost {
		t.Fatalf("one first-sight seed against the fixture archive costs %v, under the %v this test "+
			"needs to mean anything: the budget below is a fraction of that number, so a seed this "+
			"cheap would pass whether or not the read is under the lock. Grow the archive.",
			seed.cost, minSeedCost)
	}

	got := measureWorstWriterWait(store, tick)

	limit := seed.cost / 3
	if got > limit {
		t.Errorf("a competing write waited %v during the first tick over %d newly-seen sessions, each "+
			"of whose first-sight seed costs %v; want under %v — the history archive is being read "+
			"with the store lock held again. It belongs in Observer.Prime, before the lock.",
			got, seedArchiveSessions, seed.cost, limit)
	}
}

// TestShouldMeasureAWriterBlockedByASlowApply_negativeControl guards the budget
// above from going quietly vacuous the way a reader-wait budget already did.
//
// If a writer hammering the store cannot observe a deliberately slow Apply, then
// measureWorstWriterWait reports ~0 for every tick and the budget passes no matter
// what the tick holds the lock across.
func TestShouldMeasureAWriterBlockedByASlowApply_negativeControl(t *testing.T) {
	store := state.New("")
	const hold = 200 * time.Millisecond

	got := measureWorstWriterWait(store, func() {
		store.Apply(func(map[int]*state.Session) { time.Sleep(hold) })
	})

	if got < hold/2 {
		t.Errorf("a competing write measured %v while an Apply demonstrably held the lock for %v, so "+
			"measureWorstWriterWait cannot see a slow Apply and every budget written against it passes "+
			"vacuously", got, hold)
	}
}

func TestShouldEnumerateTheTerminalOncePerTickWhateverTheSessionCount(t *testing.T) {
	_, loc, _, tick := reconcileFixture(t, 8)
	tick()

	// The whole point of the batch seam: the cost of a tick must not scale with
	// the number of sessions. Before this change an 8-session box paid 8 terminal
	// enumerations per tick (times one fork per mux).
	snapshots, locates := loc.counts()
	if snapshots != 1 {
		t.Errorf("terminal enumerated %d times for 8 sessions, want exactly 1", snapshots)
	}
	if locates != 0 {
		t.Errorf("fell back to per-session Locate %d times, want 0 while a batch path exists", locates)
	}
}

// The two enumeration budgets below budget the WRITER side, for the same reason
// TestShouldNotHoldTheStoreLockAcrossTheFanoutSeed does: publish-and-swap took
// readers off the lock entirely, so the number measureWorstReaderWait reports is
// ~0 whatever the tick holds the lock across. Both of these were reader-side
// until that landed, and both went vacuous the moment it did — proven by moving
// enumerateForResolve back inside the tick's store.Apply, the exact pre-#57
// defect the first one is named for, and watching it still PASS. On the writer
// probe the same mutation fails at ~299ms against a 100ms limit.
//
// This is the second instrument in this file to be hollowed out by that change
// (see locateOnlyLocator.Locate for the first). The rule the two share: after
// publish-and-swap, ONLY a writer can observe the store lock, so any assertion
// about what a lock hold costs has to be phrased as a competing write.

func TestShouldNotHoldTheStoreLockAcrossTerminalEnumeration(t *testing.T) {
	store, _, _, tick := reconcileFixture(t, 8)

	got := measureWorstWriterWait(store, tick)

	// Generous: the assertion that matters is that the lock is not held for
	// anything like the enumeration's duration. Before the hoist this test would
	// have measured roughly 8*enumDelay, since every session's enumeration
	// happened inside store.Apply.
	limit := enumDelay / 3
	if got > limit {
		t.Errorf("a competing write waited %v during a tick whose enumeration takes %v; "+
			"want under %v — the enumeration is being held under the lock again", got, enumDelay, limit)
	}
}

func TestShouldNotHoldTheStoreLockAcrossEnumerationOnTheWMLayoutPath(t *testing.T) {
	store, loc, resolver, _ := reconcileFixture(t, 8)

	got := measureWorstWriterWait(store, func() {
		reresolveAll(context.Background(), store, resolver, nil)
	})

	// Found in production, not in a unit test: hoisting only the reconciler left
	// this path resolving per-session under the lock. On a live 12-session box the
	// 5s spike train disappeared and was replaced by sub-second stalls up to
	// 776ms, fired by the layout events this path serves. Both paths must obey the
	// same rule, so both are pinned.
	limit := enumDelay / 3
	if got > limit {
		t.Errorf("a competing write waited %v during a layout re-resolve whose enumeration "+
			"takes %v; want under %v", got, enumDelay, limit)
	}
	if snapshots, locates := loc.counts(); snapshots != 1 || locates != 0 {
		t.Errorf("layout re-resolve did %d batch enumerations and %d per-session locates, want 1 and 0",
			snapshots, locates)
	}
}

func TestShouldFallBackToPerSessionResolveWhenTheBackendCannotBatch(t *testing.T) {
	// The none backend DOES implement Snapshotter (owning nothing is a complete
	// answer), so this needs a locator that genuinely has none. The tick must still
	// resolve it — degraded in speed, never in correctness — and must do that
	// resolving before the lock, not inside it.
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[6000] = &state.Session{PID: 6000, TTY: "/dev/pts/1", CWD: "/home/test", StartedAt: time.Now()}
	})
	loc := &locateOnlyLocator{store: store, panes: map[string]terminal.PaneRef{
		"/dev/pts/1": {Backend: "locateonly", TTY: "/dev/pts/1", PaneID: 42, Title: "resolved"},
	}}
	manager := stubManager{}
	stack := detect.Stack{OSProc: fakeProcSource{st: map[int]procState{6000: procAlive}}, Terminal: loc, WM: manager}
	resolver := mapping.NewResolver(loc, manager)
	rstate := newReconcileState(fanout.NewObserver(t.TempDir()), nil)

	reconcileOnce(context.Background(), store, resolver, manager, stack,
		statustune.Default(), nil, rstate, func(int) {}, nil)

	snap := store.Snapshot()
	if len(snap.Sessions) != 1 || snap.Sessions[0].PID != 6000 {
		t.Fatalf("session did not survive a no-batch tick: %+v", snap.Sessions)
	}
	if loc.locatesDuringApply() != 0 {
		t.Errorf("the no-batch fallback ran %d Locates INSIDE store.Apply, want 0 — "+
			"that is the O(sessions)-subprocess-under-the-lock pathology this seam exists to remove",
			loc.locatesDuringApply())
	}
	if loc.locateCount() == 0 {
		t.Error("the no-batch fallback never called Locate, so a backend without a batch path resolves nothing at all")
	}
}

// A transient enumeration failure — a wedged mux, EMFILE, a slow WM socket — must
// NOT be treated as "this backend has no batch path". Doing so put a fork of the
// terminal enumeration per session back under the exclusive store lock, silently,
// on any blip: precisely the behavior measured at p99 166ms / worst 1382ms that
// this whole change set exists to remove. Resolving nothing for one tick is
// strictly better, and the next tick recovers.
func TestShouldNotResolvePerSessionWhenTheBatchEnumerationFails(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		for i := range 8 {
			pid := 6100 + i
			m[pid] = &state.Session{PID: pid, TTY: ttyName(i), CWD: "/home/test", StartedAt: time.Now()}
		}
	})
	procs := map[int]procState{}
	for i := range 8 {
		procs[6100+i] = procAlive
	}
	loc := &locateOnlyLocator{snapErr: errors.New("mux socket went away")}
	manager := stubManager{}
	stack := detect.Stack{OSProc: fakeProcSource{st: procs}, Terminal: loc, WM: manager}
	resolver := mapping.NewResolver(loc, manager)
	rstate := newReconcileState(fanout.NewObserver(t.TempDir()), nil)

	reconcileOnce(context.Background(), store, resolver, manager, stack,
		statustune.Default(), nil, rstate, func(int) {}, nil)

	if got := loc.locateCount(); got != 0 {
		t.Errorf("a failed batch enumeration fell back to %d per-session Locates, want 0 "+
			"(a transient failure must skip the tick, not fork per session)", got)
	}
	if got := len(store.Snapshot().Sessions); got != 8 {
		t.Errorf("sessions after a degraded tick = %d, want 8 — a failed enumeration must not drop anyone", got)
	}
}

// locateOnlyLocator is a backend whose Snapshot behavior is configurable and
// whose Locate records whether it was called while the store lock was held. It
// implements Snapshotter so it can model a FAILING batch path; leave snapErr nil
// and it reports ErrNoBatchPath instead, modelling a backend that has none.
type locateOnlyLocator struct {
	mu      sync.Mutex
	locates int
	inApply int
	store   *state.Store
	panes   map[string]terminal.PaneRef
	snapErr error
}

func (l *locateOnlyLocator) Name() string    { return "locateonly" }
func (l *locateOnlyLocator) Available() bool { return true }

func (l *locateOnlyLocator) Snapshot(context.Context) (map[string]terminal.PaneRef, error) {
	if l.snapErr != nil {
		return nil, l.snapErr
	}
	return nil, terminal.ErrNoBatchPath
}

// Locate detects the store lock by trying to take it as a WRITER without
// blocking: an Apply that cannot complete promptly means someone else holds the
// lock, which — since the only other writer in this test is the tick's own Apply —
// means this call is inside it.
//
// This used to probe with Snapshot, and that stopped working the moment Snapshot
// became a lock-free load of a published pointer: the probe then always succeeded
// instantly, inApply was always 0, and the assertion below passed no matter what
// the code did. A test that cannot fail is worse than no test, so the probe now
// asks the question in the only terms that still mean anything — is the WRITE lock
// held?
func (l *locateOnlyLocator) Locate(_ context.Context, tty string) (*terminal.PaneRef, error) {
	l.mu.Lock()
	l.locates++
	if l.store != nil && !writableNow(l.store) {
		l.inApply++
	}
	l.mu.Unlock()
	if ref, ok := l.panes[tty]; ok {
		return &ref, nil
	}
	return nil, nil
}

func (l *locateOnlyLocator) Activate(context.Context, *terminal.PaneRef) error { return nil }

func (l *locateOnlyLocator) locateCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locates
}

func (l *locateOnlyLocator) locatesDuringApply() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inApply
}

// writableNow reports whether the store lock is free, by trying to take it with
// an empty Apply. An empty Apply mutates nothing, so its change check finds no
// change and it neither broadcasts nor persists.
func writableNow(store *state.Store) bool {
	done := make(chan struct{})
	go func() {
		store.Apply(func(map[int]*state.Session) {})
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

// TestShouldDetectAHeldStoreLock_negativeControl guards the probe above from
// going quietly vacuous, the way its Snapshot-based predecessor did when Snapshot
// stopped taking the lock. If this stops reporting a held lock, then
// locatesDuringApply is measuring nothing and the fallback test that reads it
// proves nothing.
func TestShouldDetectAHeldStoreLock_negativeControl(t *testing.T) {
	store := state.New("")

	if !writableNow(store) {
		t.Fatal("an idle store reported its lock as held; the probe is stuck on false and would " +
			"accuse every caller of running inside an Apply")
	}

	held := make(chan struct{})
	released := make(chan struct{})
	go func() {
		store.Apply(func(map[int]*state.Session) {
			close(held)
			<-released
		})
	}()

	<-held
	got := writableNow(store)
	close(released)

	if got {
		t.Error("the probe reported the lock free while an Apply was demonstrably holding it, " +
			"so locatesDuringApply can never observe a read taken under the lock")
	}
}

// overlapDetectingLocator records whether two enumerations were ever in flight
// at the same moment.
type overlapDetectingLocator struct {
	mu       sync.Mutex
	inFlight int
	overlap  bool
	panes    map[string]terminal.PaneRef
}

func (l *overlapDetectingLocator) Name() string    { return "overlap" }
func (l *overlapDetectingLocator) Available() bool { return true }

func (l *overlapDetectingLocator) Snapshot(context.Context) (map[string]terminal.PaneRef, error) {
	l.mu.Lock()
	l.inFlight++
	if l.inFlight > 1 {
		l.overlap = true
	}
	l.mu.Unlock()

	time.Sleep(enumDelay)

	l.mu.Lock()
	l.inFlight--
	l.mu.Unlock()
	return l.panes, nil
}

func (l *overlapDetectingLocator) Locate(context.Context, string) (*terminal.PaneRef, error) {
	return nil, nil
}
func (l *overlapDetectingLocator) Activate(context.Context, *terminal.PaneRef) error { return nil }

func (l *overlapDetectingLocator) overlapped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.overlap
}

// runConcurrentResolves drives the reconciler tick and the WM layout re-resolve
// at the same time through the given turn, and reports whether their
// enumerations overlapped.
func runConcurrentResolves(t *testing.T, turn *resolveTurn) bool {
	t.Helper()

	store := state.New("")
	panes := map[string]terminal.PaneRef{"/dev/pts/7": {Backend: "overlap", TTY: "/dev/pts/7"}}
	store.Apply(func(m map[int]*state.Session) {
		m[7000] = &state.Session{PID: 7000, TTY: "/dev/pts/7", CWD: "/home/test", StartedAt: time.Now()}
	})

	loc := &overlapDetectingLocator{panes: panes}
	manager := stubManager{}
	stack := detect.Stack{OSProc: fakeProcSource{st: map[int]procState{7000: procAlive}}, Terminal: loc, WM: manager}
	resolver := mapping.NewResolver(loc, manager)
	rstate := newReconcileState(fanout.NewObserver(t.TempDir()), nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		turn.Do(func() {
			reconcileOnce(context.Background(), store, resolver, manager, stack,
				statustune.Default(), nil, rstate, func(int) {}, nil)
		})
	}()
	go func() {
		defer wg.Done()
		reresolveAll(context.Background(), store, resolver, turn)
	}()
	wg.Wait()

	return loc.overlapped()
}

func TestShouldNotEnumerateConcurrentlyFromBothResolvePaths(t *testing.T) {
	// Both paths now sample BEFORE taking the store lock, which is what made them
	// fast and also what let an enumeration taken at T0 land after one taken at
	// T1 > T0 — reverting a chip's workspace to the older reading. Serializing
	// enumerate-and-apply means the loser waits and then re-enumerates fresh, so
	// the last write always carries the freshest observation. It also stops the two
	// paths doing duplicate concurrent enumerations, the very cost being removed.
	if runConcurrentResolves(t, &resolveTurn{}) {
		t.Error("the reconciler and the WM layout path enumerated concurrently while sharing " +
			"a turn; an older enumeration can then land after a newer one")
	}
}

func TestShouldOverlapWithoutATurn_negativeControl(t *testing.T) {
	// Guards the test above from becoming vacuous: without the turn the same
	// harness MUST show the overlap. If this ever stops overlapping, the harness
	// has stopped exercising concurrency and the assertion above proves nothing.
	if !runConcurrentResolves(t, nil) {
		t.Error("the harness did not produce concurrent enumerations even without a turn, " +
			"so the serialization test above is not proving anything")
	}
}
