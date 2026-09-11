package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/detect"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// The publish gate and the history log are two independent outputs of one
// reconcile tick, and only ONE of them is allowed to be suppressed.
// Store.Apply always runs the mutation; only broadcast and persist sit behind
// the change key (state.go adoptPublishedLocked). Every sink.Record call lives
// in the reconcile tick, a hook handler, or the provider observation loop —
// none of them ask Apply whether it decided to publish.
//
// That independence is load-bearing rather than incidental:
// switchboard-dashboard reads history day-files ONLY (via `switchboard-ctl
// timeline --json`), never the socket and never state.json. So the day a
// sink.Record is moved inside a `if changed` branch — or a future change-key
// normalization tempts someone to gate "the whole tick" instead of just the
// publish — the bars keep working, every test about state keeps passing, and
// the dashboard silently loses events with nothing failing anywhere.
//
// These tests are that alarm. They are deliberately a PAIR: the suppression
// case proves history survives a suppressed publish, and the change case
// proves the same harness really does observe broadcasts, so the suppression
// case cannot pass by simply being blind to the channel.
//
// The reconcile tick records from two structurally different places, and both
// are covered here because a regression could hit either alone:
//
//   - OUTSIDE store.Apply, in the prepare phase: usage_sample and
//     session_label (fanout.go, called from main.go before Apply is entered).
//     Covered by the usage_sample tests below.
//   - INSIDE the Apply closure, under the state lock: session_end (endSession),
//     suspend/resume, and focus (applyFocus). Covered by the focus tests below.
//
// The in-Apply group is the harder half to reach, because each of those three
// records on a condition that is ITSELF the wire change that defeats
// suppression: map membership for session_end, `suspended` for suspend/resume,
// `focused` for focus. Fire the trigger and the change key moves by
// construction. There is exactly one configuration that separates them — see
// sharedWindowFixture — and the general contract underneath all three is
// pinned separately by TestShouldRunTheApplyClosureAndItsRecordsWhenTheChangeKeyIsUnchanged.

// newRecordingSink returns a sink writing day-files into dir, plus the func that
// closes it. Closing is what FLUSHES the writer goroutine, so it has to happen
// explicitly before a test reads the day-file — it cannot simply live in
// t.Cleanup, and history.Sink.Close is not idempotent so it cannot be called
// from both places either.
//
// Hence the guard: the close runs exactly once, from whichever comes first —
// the test's explicit pre-read flush, or cleanup on a t.Fatalf path that never
// reached it. Without it a Fatalf abandons the writer goroutine, which can then
// create the day-file while t.TempDir's RemoveAll is walking the same
// directory, burying the real assertion failure under a "TempDir RemoveAll
// cleanup: directory not empty" line.
//
// Registering the cleanup AFTER dir was created by t.TempDir matters: cleanups
// run LIFO, so this one closes the writer before that RemoveAll starts.
func newRecordingSink(t *testing.T, dir string) (*history.Sink, func()) {
	t.Helper()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: dir})
	closed := false
	flush := func() {
		if closed {
			return
		}
		closed = true
		sink.Close()
	}
	t.Cleanup(flush)
	return sink, flush
}

// suppressionFixture wires reconcileOnce over fakes with one Claude session
// whose transcript the test controls. The terminal is the none backend and the
// WM is a stub, so a tick resolves nothing that could move between calls: the
// only thing that can change the wire snapshot is what the test changes.
//
// The tick writes to a real history.Sink over histDir, exactly as the daemon
// does. The sink itself stays inside the fixture; what comes back is
// flushHistory, which callers invoke before reading the day-file (see
// newRecordingSink for why that is a func rather than a bare Close).
func suppressionFixture(t *testing.T, pid int, procs map[int]procState) (
	store *state.Store, flushHistory func(), histDir, transcriptPath string, tick func()) {
	t.Helper()

	// label.RawName reads ~/.claude/sessions/<pid>.json; keep the tick off the
	// developer's real home so the label cursor is driven only by this fixture.
	t.Setenv("HOME", t.TempDir())

	transcriptPath = filepath.Join(t.TempDir(), "transcript.jsonl")
	writeLines(t, transcriptPath, `{"type":"system"}`)

	histDir = t.TempDir()
	sink, flushHistory := newRecordingSink(t, histDir)

	store = state.New("") // empty path: persist is a no-op, this is about publish
	store.Apply(func(m map[int]*state.Session) {
		m[pid] = &state.Session{
			PID: pid, TTY: "/dev/pts/9", CWD: "/home/u/proj", StartedAt: time.Now(),
			Agent: state.AgentKindClaude,
			Claude: &state.AgentInfo{
				SessionID: "s-suppress", Transcript: transcriptPath, Status: state.StatusIdle,
			},
		}
	})

	loc := terminal.NewNone()
	manager := stubManager{}
	stack := detect.Stack{OSProc: fakeProcSource{st: procs}, Terminal: loc, WM: manager}
	resolver := mapping.NewResolver(loc, manager)
	rstate := newReconcileState(fanout.NewObserver(t.TempDir()))

	tick = func() {
		reconcileOnce(context.Background(), store, resolver, manager, stack,
			statustune.Default(), sink, rstate, func(int) {})
	}
	return store, flushHistory, histDir, transcriptPath, tick
}

// observableSnapshot renders everything a subscriber can see EXCEPT updated_at,
// which snapshotLocked re-stamps from the wall clock on every snapshot and
// which the change key drops by hand for exactly that reason. Two ticks whose
// bytes match here are the ticks the publish gate exists to suppress, so the
// suppression tests assert on this rather than assuming the gate fired.
func observableSnapshot(t *testing.T, store *state.Store) string {
	t.Helper()
	raw, err := json.Marshal(store.Snapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	delete(doc, "updated_at")
	normalized, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal snapshot: %v", err)
	}
	return string(normalized)
}

// awaitBroadcast waits at most d for one frame. Store.broadcast runs
// synchronously inside Apply, so by the time a tick returns the frame is
// already queued — d is slack for a loaded scheduler, never the thing being
// measured, and it is a bounded select rather than a sleep so the positive
// case returns the instant the frame lands.
func awaitBroadcast(ch <-chan state.Broadcast, d time.Duration) (state.Broadcast, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case b := <-ch:
		return b, true
	case <-timer.C:
		return state.Broadcast{}, false
	}
}

const broadcastWait = 250 * time.Millisecond

// appendUsageLines grows a Claude transcript by assistant messages carrying
// token usage — the input observeUsage samples on the next tick.
func appendUsageLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append to transcript: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("append to transcript: %v", err)
		}
	}
}

func TestShouldRecordHistoryEventsWhenApplySuppressesThePublish(t *testing.T) {
	const pid = 8100
	store, flushHistory, histDir, transcriptPath, tick := suppressionFixture(t, pid, map[int]procState{pid: procAlive})

	// Tick one settles the session (label emitted, usage cursor primed to EOF)
	// and publishes whatever it changed. Everything asserted below is about the
	// SECOND tick, so subscribe after this one.
	tick()

	before := observableSnapshot(t, store)
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	// The session burns tokens without anything a bar could render changing:
	// no status edge, no window move, no new session. This is the overwhelmingly
	// common shape of a live tick — 6686 of today's 8000-odd events are
	// usage_samples — and it is precisely the tick the publish gate suppresses.
	appendUsageLines(t, transcriptPath, assistantUsageModelLine("claude-opus-4-8", 100, 40))
	tick()

	// Guard the guard: if a later change ever makes this tick move the wire
	// snapshot, the publish is no longer being suppressed and the assertions
	// below stop testing the invariant they were written for.
	if after := observableSnapshot(t, store); after != before {
		t.Fatalf("the tick moved the observable snapshot, so no publish was suppressed and this "+
			"test is not exercising the invariant\nbefore: %s\nafter:  %s", before, after)
	}

	if b, ok := awaitBroadcast(sub, broadcastWait); ok {
		t.Errorf("Apply broadcast a frame for a tick that changed nothing observable: %s", b.JSON)
	}

	flushHistory()
	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 1 {
		t.Fatalf("got %d usage_sample events from a suppressed tick, want 1 — history recording "+
			"has been made conditional on Apply deciding to publish, and the dashboard (which reads "+
			"history day-files only) will silently lose events: %+v", len(samples), samples)
	}
	if samples[0].TokIn != 100 || samples[0].TokOut != 40 {
		t.Errorf("usage_sample = %+v, want the 100/40 delta the transcript accrued", samples[0])
	}
	if samples[0].SessionID != "s-suppress" || samples[0].PID != pid {
		t.Errorf("usage_sample identity = %+v, want session s-suppress / pid %d", samples[0], pid)
	}
}

// The positive control for the test above. Same fixture, same tick, but the
// world actually moves: the process dies, so the reconciler's liveness sweep
// closes the lane. History and the publish must BOTH fire. Without this, a
// harness that had quietly stopped delivering broadcasts at all — a subscribe
// that never registered, a channel read from the wrong store — would leave the
// suppression assertion passing while proving nothing.
func TestShouldBroadcastWhenTheChangeKeyChanges(t *testing.T) {
	const pid = 8101
	procs := map[int]procState{pid: procAlive}
	store, flushHistory, histDir, _, tick := suppressionFixture(t, pid, procs)

	tick()

	before := observableSnapshot(t, store)
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	procs[pid] = procGone
	tick()

	if after := observableSnapshot(t, store); after == before {
		t.Fatalf("the observable snapshot did not move after the session's process died, so this "+
			"control is not exercising a publish: %s", after)
	}

	b, ok := awaitBroadcast(sub, broadcastWait)
	if !ok {
		t.Fatal("no broadcast for a tick that closed a session's lane; the publish gate is " +
			"suppressing a real change")
	}
	if len(b.Snapshot.Sessions) != 0 {
		t.Errorf("broadcast carried %d sessions, want the dead session dropped: %+v",
			len(b.Snapshot.Sessions), b.Snapshot.Sessions)
	}

	flushHistory()
	ends := eventsOfType(readEvents(t, histDir), history.EventSessionEnd)
	if len(ends) != 1 {
		t.Fatalf("got %d session_end events, want exactly 1: %+v", len(ends), ends)
	}
}

// Suppression is not a one-shot: the daemon sits in it for hours. Every tick in
// that stretch still owes the dashboard its distinct messages, independently of
// publishes. Repeated streamed fragments of one message must not be counted twice.
func TestShouldRecordEveryTicksHistoryAcrossAStretchOfSuppressedPublishes(t *testing.T) {
	const pid = 8102
	store, flushHistory, histDir, transcriptPath, tick := suppressionFixture(t, pid, map[int]procState{pid: procAlive})

	tick()

	before := observableSnapshot(t, store)
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	const ticks = 3
	for i := range ticks {
		line := fmt.Sprintf(`{"type":"assistant","message":{"id":"msg-%d","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":10,"output_tokens":4}}}`, i)
		appendUsageLines(t, transcriptPath, line, line)
		tick()
	}

	if after := observableSnapshot(t, store); after != before {
		t.Fatalf("the observable snapshot moved during the run, so at least one publish was not "+
			"suppressed\nbefore: %s\nafter:  %s", before, after)
	}
	if b, ok := awaitBroadcast(sub, broadcastWait); ok {
		t.Errorf("Apply broadcast a frame during a stretch of no-op ticks: %s", b.JSON)
	}

	flushHistory()
	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != ticks {
		t.Fatalf("got %d usage_sample events from %d suppressed ticks, want one per tick: %+v",
			len(samples), ticks, samples)
	}
}

// focusManager is a wm.Manager with a fixed active-window address, so a test can
// drive applyFocus's real input. stubManager always reports "" (nothing
// focused), which is why it cannot exercise the focus path at all.
type focusManager struct{ addr string }

func (focusManager) Name() string                                       { return "focus" }
func (focusManager) Available() bool                                    { return true }
func (focusManager) Clients(context.Context) ([]wm.Window, error)       { return nil, nil }
func (m focusManager) ActiveWindow(context.Context) (string, error)     { return m.addr, nil }
func (focusManager) Focus(context.Context, string) error                { return nil }
func (focusManager) Subscribe(context.Context) (<-chan wm.Event, error) { return nil, nil }

// sharedWindowFixture puts TWO agent sessions behind ONE window address, which
// is what two wezterm splits (or tabs) in a single Hyprland window actually look
// like to the daemon — a routine local layout, not a contrived one.
//
// It is the one configuration in which an in-Apply sink.Record fires while the
// change key holds still, and it is worth spelling out why, because the test
// below is only meaningful if this stays true:
//
// applyFocus (main.go) recovers the previously-focused id by ranging the session
// map and breaking at the first Focused session, then assigns the new id while
// ranging the map a SECOND time, keeping the last Focused session it sees. With
// two sessions focused, the first range yields one of them and the second range
// yields the other, each order independently randomized by the runtime — so
// prevID != newID on roughly half of all ticks and a focus event is recorded.
// Meanwhile both sessions were already Focused and stay Focused, so not one byte
// of the wire snapshot moves and Apply suppresses the publish.
//
// NOTE FOR WHOEVER FIXES THAT: the spurious focus event is a real daemon bug
// (the dashboard's focus spans get chopped up by edges the user never caused).
// It should be fixed. When it is, this test will go red with "0 focus events" —
// that is the fix working, NOT this guard failing. Convert it to a t.Skip naming
// the fix rather than deleting it, and lean on
// TestShouldRunTheApplyClosureAndItsRecordsWhenTheChangeKeyIsUnchanged, which
// pins the same invariant without depending on the bug.
//
// tick takes the sink per call so the settle ticks can run against a DISABLED
// sink: the day-file then contains only the events of the measured stretch, and
// an assertion on "did anything get recorded" cannot be satisfied by a settle
// tick that ran before the measurement began.
func sharedWindowFixture(t *testing.T) (store *state.Store, tick func(sink *history.Sink)) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	const sharedAddr = "0xshared"
	store = state.New("")
	store.Apply(func(m map[int]*state.Session) {
		for i, sid := range []string{"s-left", "s-right"} {
			pid := 8200 + i
			m[pid] = &state.Session{
				PID: pid, CWD: "/home/u/proj", StartedAt: time.Now(),
				Agent: state.AgentKindClaude,
				// Already focused: the window is active and both panes are in it.
				// The tick must therefore flip no flag, which is the whole point.
				Focused:  true,
				Hyprland: &state.HyprlandInfo{Address: sharedAddr},
				Claude:   &state.AgentInfo{SessionID: sid, Status: state.StatusIdle},
			}
		}
	})

	loc := terminal.NewNone()
	manager := focusManager{addr: sharedAddr}
	stack := detect.Stack{OSProc: fakeProcSource{st: map[int]procState{}}, Terminal: loc, WM: manager}
	resolver := mapping.NewResolver(loc, manager)
	rstate := newReconcileState(fanout.NewObserver(t.TempDir()))

	tick = func(sink *history.Sink) {
		reconcileOnce(context.Background(), store, resolver, manager, stack,
			statustune.Default(), sink, rstate, func(int) {})
	}
	return store, tick
}

// drainBroadcasts counts every frame already queued on a subscription, waiting
// briefly for a first one so a frame in flight is not missed.
func drainBroadcasts(sub <-chan state.Broadcast) int {
	frames := 0
	for {
		if _, ok := awaitBroadcast(sub, 10*time.Millisecond); !ok {
			return frames
		}
		frames++
	}
}

// The in-Apply half of the invariant. usage_sample proves history survives a
// suppressed publish from the PREPARE phase, which runs before Apply is entered
// and so never sees the publish decision at all. This proves it for a Record
// made from INSIDE the Apply closure, under the state lock, in the same call
// that decides to suppress — the placement where "gate the record on the publish
// decision" is an easy and invisible mistake to make.
//
// focus is the event class this protects: 698 of today's ~8200 events, and the
// input to the dashboard's focus spans.
func TestShouldRecordFocusFromInsideApplyWhenApplySuppressesThePublish(t *testing.T) {
	store, tick := sharedWindowFixture(t)

	// Settle (labels emitted, capabilities published, flags reconciled) against a
	// disabled sink, so every event in histDir below belongs to the measured
	// stretch and nothing can pass on the strength of a pre-measurement tick.
	tick(history.NewSink(history.Config{}))

	histDir := t.TempDir()
	sink, flushHistory := newRecordingSink(t, histDir)

	before := observableSnapshot(t, store)
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	// Roughly half of these ticks record a focus event (see sharedWindowFixture);
	// over this many, "none at all" is not a scheduling accident, it is the
	// recorder having been removed or gated.
	const ticks = 40
	for range ticks {
		tick(sink)
	}

	if after := observableSnapshot(t, store); after != before {
		t.Fatalf("the observable snapshot moved across %d focus ticks, so the publishes were not "+
			"suppressed and this test is not exercising the invariant\nbefore: %s\nafter:  %s",
			ticks, before, after)
	}
	if frames := drainBroadcasts(sub); frames != 0 {
		t.Errorf("Apply broadcast %d frames across %d ticks that changed nothing observable", frames, ticks)
	}

	flushHistory()
	focus := eventsOfType(readEvents(t, histDir), history.EventFocus)
	if len(focus) == 0 {
		t.Fatalf("no focus events from %d suppressed ticks — an in-Apply sink.Record has been made "+
			"conditional on Apply deciding to publish, and the dashboard (which reads history "+
			"day-files only) will silently lose focus spans. If applyFocus's spurious-edge bug was "+
			"just fixed, see the note on sharedWindowFixture: skip this test, do not delete it.",
			ticks)
	}
	for _, ev := range focus {
		if ev.SessionID != "s-left" && ev.SessionID != "s-right" {
			t.Errorf("focus event names session %q, want one of the two sessions in the window: %+v",
				ev.SessionID, ev)
		}
	}
}

// The contract every in-Apply recorder rests on, pinned directly and without
// depending on any particular caller: Apply runs the mutation UNCONDITIONALLY
// and only broadcast/persist sit behind the change key (state.go:554). A
// plausible-looking "optimization" — skip the closure when we can tell the
// publish would be suppressed — would take every in-Apply record with it:
// session_end, suspend/resume and focus all at once, silently.
//
// The mutation here is the one the daemon really does make on a quiet tick:
// the resolver re-samples the pane title and stamps it every reconcile
// (mapping.weztermInfo). Both fields are json:"-", so the wire snapshot cannot
// move and the publish must be suppressed — while the closure still has to run
// and its record still has to land.
func TestShouldRunTheApplyClosureAndItsRecordsWhenTheChangeKeyIsUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	histDir := t.TempDir()
	sink, flushHistory := newRecordingSink(t, histDir)

	const pid = 8300
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[pid] = &state.Session{
			PID: pid, CWD: "/home/u/proj", StartedAt: time.Now(), Agent: state.AgentKindClaude,
			Wezterm: &state.WeztermInfo{WindowTitle: "proj", Title: "idle", TitleAt: time.Now()},
			Claude:  &state.AgentInfo{SessionID: "s-apply", Status: state.StatusIdle},
		}
	})

	before := observableSnapshot(t, store)
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	ran := false
	store.Apply(func(m map[int]*state.Session) {
		sess := m[pid]
		if sess == nil {
			return
		}
		ran = true
		sess.Wezterm.Title = "* working"
		sess.Wezterm.TitleAt = time.Now()
		sink.Record(history.Event{Ts: time.Now(), Type: history.EventFocus,
			SessionID: "s-apply", PID: pid, Agent: state.AgentKindClaude})
	})

	if !ran {
		t.Fatal("Apply did not run its mutation; every in-Apply history record is now dead code")
	}
	if after := observableSnapshot(t, store); after != before {
		t.Fatalf("the mutation moved the observable snapshot, so no publish was suppressed and "+
			"this test is not exercising the invariant\nbefore: %s\nafter:  %s", before, after)
	}
	if b, ok := awaitBroadcast(sub, broadcastWait); ok {
		t.Errorf("Apply broadcast a frame for a mutation confined to json:\"-\" fields: %s", b.JSON)
	}

	flushHistory()
	if focus := eventsOfType(readEvents(t, histDir), history.EventFocus); len(focus) != 1 {
		t.Fatalf("got %d focus events from a suppressed Apply, want 1 — a record made inside the "+
			"Apply closure did not reach the day-file: %+v", len(focus), focus)
	}
}
