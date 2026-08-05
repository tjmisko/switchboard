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
// The transcript is deleted after sampling, so an inline read would find nothing
// and leave the chip green.
func TestSelfHealStuckStatusUsesThePreLockSample(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	sess, m, tpath := signalEnv(t, state.StatusWorking, since)

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(sm map[int]*state.Session) { sm[sess.PID] = sess })
	signals := sampleSignals(store, testTune)

	if err := os.Remove(tpath); err != nil {
		t.Fatal(err)
	}

	selfHealStuckStatus(m, time.Now(), testTune, nil, signals)

	if sess.Claude.Status != state.StatusIdle {
		t.Fatalf("status = %q, want idle from the sampled interrupt notice; an inline read would have found no file",
			sess.Claude.Status)
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
	signals := sampleSignals(store, testTune)

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

// This is where the staleness guard actually earns its place, and it took a
// failed mutation to find it.
//
// On the stuck-status path the guard looks load-bearing but is not: a sampled
// signal that predates a new StatusSince is rejected downstream anyway by
// `ts.After(c.StatusSince)`. The permission path has no such second line of
// defence. ResolveKind bakes `since` INTO its answer — it returns a resolution
// only if one landed after the reference time — and permissionExit then consumes
// that answer without re-checking any timestamp.
//
// So if the chip re-latches red on a NEW prompt between the sample and the lock,
// a stale sample carries a resolution belonging to the PREVIOUS prompt, and the
// new red chip is cleared by evidence that has nothing to do with it. The guard
// rejects it and the inline re-read, measured against the new StatusSince, finds
// nothing and correctly keeps the chip red.
func TestSelfHealStaleAttentionRejectsAResolutionBelongingToAPriorPrompt(t *testing.T) {
	firstPrompt := mustParse(t, "2026-06-01T21:39:00Z")
	// The user declined the FIRST prompt; that interrupt is what the sample sees.
	m := permMap(writeTranscript(t, tInterrupt("2026-06-01T21:39:30Z")), firstPrompt)
	sess := m[100]

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(sm map[int]*state.Session) { sm[sess.PID] = sess })
	signals := sampleSignals(store, testTune)

	// Mid-tick, a SECOND permission prompt latches the chip red again. The decline
	// above predates it and must not clear it.
	secondPrompt := mustParse(t, "2026-06-01T21:40:00Z")
	sess.Claude.StatusSince = secondPrompt

	selfHealStaleAttention(m, secondPrompt.Add(time.Minute), testTune, nil, signals)

	if got := sess.Claude.Status; got != "permission" {
		t.Fatalf("status = %q, want permission: the sampled resolution belongs to the previous prompt", got)
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

	got := sampleSignals(store, testTune)[sess.PID]
	if !got.quiescent {
		t.Fatalf("sample = %+v, want quiescent: nothing was written since the chip transitioned", got)
	}
}
