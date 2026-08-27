package main

import (
	"context"
	"encoding/json"
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

// suppressionFixture wires reconcileOnce over fakes with one Claude session
// whose transcript the test controls. The terminal is the none backend and the
// WM is a stub, so a tick resolves nothing that could move between calls: the
// only thing that can change the wire snapshot is what the test changes.
//
// It returns the store, the real history sink (writing day-files into histDir,
// exactly as the daemon does), the transcript path, and the tick.
func suppressionFixture(t *testing.T, pid int, procs map[int]procState) (
	store *state.Store, sink *history.Sink, histDir, transcriptPath string, tick func()) {
	t.Helper()

	// label.RawName reads ~/.claude/sessions/<pid>.json; keep the tick off the
	// developer's real home so the label cursor is driven only by this fixture.
	t.Setenv("HOME", t.TempDir())

	transcriptPath = filepath.Join(t.TempDir(), "transcript.jsonl")
	writeLines(t, transcriptPath, `{"type":"system"}`)

	histDir = t.TempDir()
	// Not registered with t.Cleanup: Sink.Close is not idempotent, and every test
	// below closes it explicitly to flush the writer before reading the day-file.
	sink = history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})

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
	return store, sink, histDir, transcriptPath, tick
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
	store, sink, histDir, transcriptPath, tick := suppressionFixture(t, pid, map[int]procState{pid: procAlive})

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

	sink.Close()
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
	store, sink, histDir, _, tick := suppressionFixture(t, pid, procs)

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

	sink.Close()
	ends := eventsOfType(readEvents(t, histDir), history.EventSessionEnd)
	if len(ends) != 1 {
		t.Fatalf("got %d session_end events, want exactly 1: %+v", len(ends), ends)
	}
}

// Suppression is not a one-shot: the daemon sits in it for hours. Every tick in
// that stretch still owes the dashboard its events, so the count must track the
// ticks and not the publishes.
func TestShouldRecordEveryTicksHistoryAcrossAStretchOfSuppressedPublishes(t *testing.T) {
	const pid = 8102
	store, sink, histDir, transcriptPath, tick := suppressionFixture(t, pid, map[int]procState{pid: procAlive})

	tick()

	before := observableSnapshot(t, store)
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	const ticks = 3
	for range ticks {
		appendUsageLines(t, transcriptPath, assistantUsageModelLine("claude-opus-4-8", 10, 4))
		tick()
	}

	if after := observableSnapshot(t, store); after != before {
		t.Fatalf("the observable snapshot moved during the run, so at least one publish was not "+
			"suppressed\nbefore: %s\nafter:  %s", before, after)
	}
	if b, ok := awaitBroadcast(sub, broadcastWait); ok {
		t.Errorf("Apply broadcast a frame during a stretch of no-op ticks: %s", b.JSON)
	}

	sink.Close()
	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != ticks {
		t.Fatalf("got %d usage_sample events from %d suppressed ticks, want one per tick: %+v",
			len(samples), ticks, samples)
	}
}
