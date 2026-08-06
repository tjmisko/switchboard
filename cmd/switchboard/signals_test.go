package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// signalEnv is a session with a transcript whose newest entry is an interrupt
// notice, i.e. the working->idle self-heal's trigger.
func signalEnv(t *testing.T, status string, statusSince time.Time) (*state.Session, map[int]*state.Session, string) {
	t.Helper()
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath,
		`{"type":"user","timestamp":"`+statusSince.Add(time.Second).UTC().Format(time.RFC3339Nano)+
			`","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`)
	// The stat gate compares mtime against StatusSince, so the file must look
	// newer than the chip.
	touch := statusSince.Add(2 * time.Second)
	if err := os.Chtimes(tpath, touch, touch); err != nil {
		t.Fatal(err)
	}
	sess := &state.Session{PID: 42, Agent: state.AgentKindClaude, CWD: "/proj",
		Claude: &state.AgentInfo{SessionID: "s42", Transcript: tpath,
			Status: status, StatusSince: statusSince}}
	return sess, map[int]*state.Session{42: sess}, tpath
}

// The point of the hoist: the decision comes from reads taken before the lock.
//
// Proving "did not read again" needs a change the sample survives, since the
// guard now rejects a sample whose transcript moved (see freshFor). Making the
// file unreadable is exactly that: os.Stat still answers, so the stamp is
// unchanged and the sample stays valid, while an inline re-read would fail to
// open the file and leave the chip green.
func TestSelfHealStuckStatusUsesThePreLockSample(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	sess, m, tpath := signalEnv(t, state.StatusWorking, since)

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(sm map[int]*state.Session) { sm[sess.PID] = sess })
	signals := sampleSignals(store.Snapshot(), testTune)

	if err := os.Chmod(tpath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(tpath, 0o644) })
	if f, err := os.Open(tpath); err == nil {
		f.Close()
		t.Skip("the transcript is still readable at mode 0000 — running as root?")
	}

	selfHealStuckStatus(m, time.Now(), testTune, nil, signals)

	if sess.Claude.Status != state.StatusIdle {
		t.Fatalf("status = %q, want idle from the sampled interrupt notice; an inline read could not have opened the file",
			sess.Claude.Status)
	}
}

// An answer that lands while the tick is doing its pre-lock sampling must be seen
// on THAT tick, not the next one.
//
// This is the one property the permission self-heal gets for free by reading at
// decision time, and it is worth pinning because an earlier revision of this branch
// lost it: it sampled the resolution before the lock, and a decline landing in the
// sampling window — during which Status stays "permission" and StatusSince stays
// put, since nothing has released the chip yet — was missed for a full extra tick.
// T9's per-writer routing put the read back at decision time for its own reasons;
// this asserts the freshness that comes with it.
func TestSelfHealStaleAttentionSeesAnAnswerThatLandsDuringTheTick(t *testing.T) {
	prompt := mustParse(t, "2026-06-01T21:39:00Z")
	// The prompt is genuinely unanswered when the tick starts.
	tpath := writeTranscript(t, tResult("2026-06-01T21:39:10Z"))
	m := permMap(tpath, prompt)
	sess := m[100]

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(sm map[int]*state.Session) { sm[sess.PID] = sess })
	sampleSignals(store.Snapshot(), testTune) // the tick's pre-lock phase runs

	// The user declines DURING it. Status, StatusSince and the path are all
	// untouched — only the file moved.
	appendTranscript(t, tpath, tInterrupt("2026-06-01T21:39:30Z"))

	healStale(t, m, prompt.Add(time.Minute), testTune, nil)

	if got := sess.Claude.Status; got == "permission" {
		t.Fatal("the chip is still red a full tick after the prompt was declined: " +
			"the decision was made against a read older than the answer")
	}
}

// A status edge landing between the sample and the lock changes what the read
// means, not just how stale it is. The guard must reject, and the inline fallback
// must then decide on the current state — here the transcript is gone, so the
// chip must be left exactly as the hook set it.
func TestSelfHealStuckStatusRejectsASampleFromABeforeStatusEdge(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	sess, m, tpath := signalEnv(t, state.StatusWorking, since)

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(sm map[int]*state.Session) { sm[sess.PID] = sess })
	signals := sampleSignals(store.Snapshot(), testTune)

	// A hook lands mid-tick: the chip went idle and re-stamped StatusSince, so the
	// sampled interrupt notice is now OLDER than the transition it would justify.
	sess.Claude.Status = state.StatusIdle
	sess.Claude.StatusSince = time.Now()
	if err := os.Remove(tpath); err != nil {
		t.Fatal(err)
	}

	selfHealStuckStatus(m, time.Now(), testTune, nil, signals)

	if sess.Claude.Status != state.StatusIdle {
		t.Fatalf("status = %q, want idle unchanged: a sample taken against the previous status must be discarded",
			sess.Claude.Status)
	}
}

func TestSelfHealStuckStatusFallsBackToAnInlineReadWithoutASample(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	sess, m, _ := signalEnv(t, state.StatusWorking, since)

	// noSignals: nothing was sampled, so the self-heal must read for itself and
	// still find the interrupt notice.
	selfHealStuckStatus(m, time.Now(), testTune, nil, noSignals)

	if sess.Claude.Status != state.StatusIdle {
		t.Fatalf("status = %q, want idle from an inline read when no sample exists", sess.Claude.Status)
	}
}

// A resolution belonging to a PREVIOUS prompt must never clear the red a NEW
// prompt just raised.
//
// The hazard is that ResolveKind bakes `since` into its answer — it reports a
// resolution only if one landed after the reference time — and permissionExit then
// consumes that answer without re-checking any timestamp. So the answer is only as
// correct as the instant it was dated against. Under T9 that instant is the
// prompt's OWN onset (c.Pending[writer].Since, falling back to StatusSince), which
// is what makes a chip that re-latches mid-tick immune to the earlier decline.
func TestSelfHealStaleAttentionRejectsAResolutionBelongingToAPriorPrompt(t *testing.T) {
	firstPrompt := mustParse(t, "2026-06-01T21:39:00Z")
	// The user declined the FIRST prompt.
	m := permMap(writeTranscript(t, tInterrupt("2026-06-01T21:39:30Z")), firstPrompt)
	sess := m[100]

	// A SECOND permission prompt latches the chip red again. The decline above
	// predates it and must not clear it.
	secondPrompt := mustParse(t, "2026-06-01T21:40:00Z")
	sess.Claude.StatusSince = secondPrompt

	healStale(t, m, secondPrompt.Add(time.Minute), testTune, nil)

	if got := sess.Claude.Status; got != "permission" {
		t.Fatalf("status = %q, want permission: that resolution belongs to the previous prompt", got)
	}
}

// The stat short-circuit is what keeps a quiescent box off the disk, so it has to
// survive the move into readSignals.
func TestSampleSignalsSkipsTheTailReadWhenNothingWasWritten(t *testing.T) {
	since := time.Now()
	sess, _, tpath := signalEnv(t, state.StatusWorking, since)
	// Make the transcript OLDER than the chip's transition.
	old := since.Add(-time.Hour)
	if err := os.Chtimes(tpath, old, old); err != nil {
		t.Fatal(err)
	}

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(sm map[int]*state.Session) { sm[sess.PID] = sess })

	got := sampleSignals(store.Snapshot(), testTune)[sess.PID]
	if !got.quiescent {
		t.Fatalf("sample = %+v, want quiescent: nothing was written since the chip transitioned", got)
	}
}
