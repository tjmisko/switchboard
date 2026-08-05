package main

import (
	"context"
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

// reconcileFixture builds a reconcileOnce call site over fakes, with sessionCount
// sessions already tracked. Claude enrichment is left nil so the tick does no
// transcript I/O — this fixture is about the resolve, not the fanout observer.
func reconcileFixture(t *testing.T, sessionCount int) (*state.Store, *slowBatchLocator, *mapping.Resolver, func()) {
	t.Helper()

	panes := make(map[string]terminal.PaneRef, sessionCount)
	store := state.New("")
	procs := map[int]procState{}
	store.Apply(func(m map[int]*state.Session) {
		for i := range sessionCount {
			pid := 5000 + i
			tty := ttyName(i)
			procs[pid] = procAlive
			m[pid] = &state.Session{PID: pid, TTY: tty, CWD: "/home/test", StartedAt: time.Now()}
			panes[tty] = terminal.PaneRef{Backend: "slowbatch", TTY: tty, PaneID: i}
		}
	})

	loc := &slowBatchLocator{panes: panes}
	manager := stubManager{}
	stack := detect.Stack{OSProc: fakeProcSource{st: procs}, Terminal: loc, WM: manager}
	resolver := mapping.NewResolver(loc, manager)
	rstate := newReconcileState(fanout.NewObserver(t.TempDir()), nil)

	tick := func() {
		reconcileOnce(context.Background(), store, resolver, manager, stack,
			statustune.Default(), nil, rstate, func(int) {})
	}
	return store, loc, resolver, tick
}

// measureWorstReaderWait runs work while a goroutine hammers store.Snapshot, and
// reports the longest a reader was blocked. Those readers stand in for what
// actually queues behind this lock in production: every waybar subscriber, every
// hook RPC from a live session, and `switchboard-ctl focus` — the chip click.
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

func ttyName(i int) string {
	return "/dev/pts/" + string(rune('0'+i))
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

func TestShouldNotHoldTheStoreLockAcrossTerminalEnumeration(t *testing.T) {
	store, _, _, tick := reconcileFixture(t, 8)

	got := measureWorstReaderWait(store, tick)

	// Generous: the assertion that matters is that a reader is not blocked for
	// anything like the enumeration's duration. Before the hoist this test would
	// have measured roughly 8*enumDelay, since every session's enumeration
	// happened inside store.Apply.
	limit := enumDelay / 3
	if got > limit {
		t.Errorf("a store reader blocked for %v during a tick whose enumeration takes %v; "+
			"want under %v — the enumeration is being held under the lock again", got, enumDelay, limit)
	}
}

func TestShouldNotHoldTheStoreLockAcrossEnumerationOnTheWMLayoutPath(t *testing.T) {
	store, loc, resolver, _ := reconcileFixture(t, 8)

	got := measureWorstReaderWait(store, func() {
		reresolveAll(context.Background(), store, resolver)
	})

	// Found in production, not in a unit test: hoisting only the reconciler left
	// this path resolving per-session under the lock. On a live 12-session box the
	// 5s spike train disappeared and was replaced by sub-second stalls up to
	// 776ms, fired by the layout events this path serves. Both paths must obey the
	// same rule, so both are pinned.
	limit := enumDelay / 3
	if got > limit {
		t.Errorf("a store reader blocked for %v during a layout re-resolve whose enumeration "+
			"takes %v; want under %v", got, enumDelay, limit)
	}
	if snapshots, locates := loc.counts(); snapshots != 1 || locates != 0 {
		t.Errorf("layout re-resolve did %d batch enumerations and %d per-session locates, want 1 and 0",
			snapshots, locates)
	}
}

func TestShouldFallBackToPerSessionResolveWhenTheBackendCannotBatch(t *testing.T) {
	// terminal.NewNone provides no Snapshotter, so SnapshotOrNil yields nil and the
	// tick must still resolve — degraded in speed, never in correctness.
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[6000] = &state.Session{PID: 6000, TTY: "/dev/pts/1", CWD: "/home/test", StartedAt: time.Now()}
	})
	loc := terminal.NewNone()
	manager := stubManager{}
	stack := detect.Stack{OSProc: fakeProcSource{st: map[int]procState{6000: procAlive}}, Terminal: loc, WM: manager}
	resolver := mapping.NewResolver(loc, manager)
	rstate := newReconcileState(fanout.NewObserver(t.TempDir()), nil)

	reconcileOnce(context.Background(), store, resolver, manager, stack,
		statustune.Default(), nil, rstate, func(int) {})

	// The session survives the tick with its identity intact; the none backend
	// simply never resolves it past the Observe tier.
	snap := store.Snapshot()
	if len(snap.Sessions) != 1 || snap.Sessions[0].PID != 6000 {
		t.Fatalf("session did not survive a no-batch tick: %+v", snap.Sessions)
	}
}
