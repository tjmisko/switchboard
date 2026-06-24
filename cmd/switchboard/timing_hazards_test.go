package main

import (
	"os"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/transcript"
)

// A running catalog of tricky timing situations in the hookless status recovery,
// each pinned as a regression. Status hooks (UserPromptSubmit, Stop, PostToolUse,
// …) and the transcript are two separate event streams the reconciler has to
// reconcile, and a fast user action that lands in the gap between a hook and the
// next exposes clock-ordering races. Every row is a scenario that bit us — or
// plausibly could — kept executable so a regression flips a chip color and fails
// the build.
//
// Each row models the real two-phase flow:
//
//  1. a status hook fires; the daemon dates the transition from the newest
//     transcript entry at that instant (transcript.AnchorTime — the skew fix),
//     NOT from wall-clock now;
//  2. the transcript keeps growing (a later interrupt, a teammate's tool_result,
//     a `!bash` line, …);
//  3. a reconcile tick runs selfHealStuckStatus and must land on the right color.
//
// The ids match the headings in docs/timing-hazards.md. Add a row AND a doc entry
// whenever a new timing hazard surfaces.
func TestTimingHazards(t *testing.T) {
	const (
		t0   = "2026-06-22T10:50:00Z" // the hook's triggering entry
		t1s  = "2026-06-22T10:50:01Z" // a near-immediate follow-up (the "quick" race)
		t30s = "2026-06-22T10:50:30Z"
		t40s = "2026-06-22T10:50:40Z"
		t2m  = "2026-06-22T10:52:00Z"
	)

	hazards := []struct {
		id        string   // matches a heading in docs/timing-hazards.md
		atHook    []string // transcript entries present when the status hook fired
		status    string   // the color the hook set
		subagents int      // teammates in flight at reconcile
		appended  []string // entries written between the hook and the reconcile
		stale     bool     // force the file mtime before StatusSince (no fresh write)
		afterHook time.Duration
		want      string
	}{
		{
			// THE bug this branch fixes: prompt turns the chip green, then a Ctrl+C
			// one second later writes an interrupt marker but fires no Stop hook. With
			// StatusSince anchored to the prompt (t0) the marker (t1s) reads as newer
			// and demotes; a wall-clock StatusSince would sit after t1s and strand it
			// green forever (nothing else is ever written).
			id:        "H1-quick-interrupt",
			atHook:    []string{tUserText(t0, "merge the prs")},
			status:    "working",
			appended:  []string{tInterrupt(t1s)},
			afterHook: 5 * time.Minute,
			want:      "idle",
		},
		{
			// The same interrupt, but minutes into a real turn. Always worked (the
			// marker is comfortably newer than the prompt) — kept as the contrast case.
			id:        "H2-slow-interrupt",
			atHook:    []string{tUserText(t0, "do a big thing")},
			status:    "working",
			appended:  []string{tAssistant(t40s), tInterrupt(t2m)},
			afterHook: 5 * time.Minute,
			want:      "idle",
		},
		{
			// A busy turn writes activity (tool_results, assistant text) but never the
			// interrupt marker, so a genuinely working session is never falsely decayed
			// — the reason the demotion keys on the marker, not a no-activity TTL.
			id:        "H3-busy-no-interrupt",
			atHook:    []string{tUserText(t0, "long job")},
			status:    "working",
			appended:  []string{tResult(t30s), tAssistant(t40s)},
			afterHook: 5 * time.Minute,
			want:      "working",
		},
		{
			// A Stop dated the chip idle from the final assistant message (t0). A later
			// `!bash` line is NOT agent activity and must not re-green the chip — and
			// the anchor must not make the final message itself read as fresh activity
			// (it is exactly AT StatusSince, not after).
			id:        "H4-local-command-after-idle",
			atHook:    []string{tAssistant(t0)},
			status:    "idle",
			appended:  []string{tBash(t30s)},
			afterHook: 5 * time.Minute,
			want:      "idle",
		},
		{
			// Orchestrator turn ended (idle), then a background teammate's tool_result
			// lands — genuine activity after the chip went idle, so it resumes to green
			// with no hook behind it.
			id:        "H5-teammate-resume-after-idle",
			atHook:    []string{tAssistant(t0)},
			status:    "idle",
			appended:  []string{tResult(t30s)},
			afterHook: 5 * time.Minute,
			want:      "working",
		},
		{
			// Delegating is decided from the in-flight-subagent count, not a transcript
			// read, so it must fire even when the main transcript is quiet (stale mtime)
			// — the case the activity pre-gate would otherwise skip.
			id:        "H6-delegating-quiet-transcript",
			atHook:    []string{tAssistant(t0)},
			status:    "idle",
			subagents: 2,
			stale:     true,
			afterHook: 5 * time.Minute,
			want:      "delegating",
		},
	}

	for _, h := range hazards {
		t.Run(h.id, func(t *testing.T) {
			// Phase 1: the daemon anchors StatusSince to the newest entry at hook time.
			since, ok := transcript.AnchorTime(writeTranscript(t, h.atHook...), transcript.DefaultTailBytes)
			if !ok {
				t.Fatalf("AnchorTime: no turn entry in atHook fixture")
			}
			reconcileNow := since.Add(h.afterHook)

			// Phase 2: the transcript grows to its reconcile-time tail.
			full := writeTranscript(t, append(append([]string{}, h.atHook...), h.appended...)...)
			mtime := reconcileNow
			if h.stale {
				mtime = since.Add(-time.Hour)
			}
			if err := os.Chtimes(full, mtime, mtime); err != nil {
				t.Fatalf("chtimes: %v", err)
			}

			// Phase 3: a reconcile tick must land on the right color.
			m := stuckMap(h.status, full, since)
			m[100].Claude.InFlightSubagents = h.subagents
			selfHealStuckStatus(m, reconcileNow, testTune)

			if got := m[100].Claude.Status; got != h.want {
				t.Errorf("status = %q, want %q", got, h.want)
			}
		})
	}
}

// tUserText is a user prompt entry — the transcript record Claude Code writes
// (and dates) just before firing UserPromptSubmit.
func tUserText(ts, s string) string {
	return `{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":[{"type":"text","text":"` + s + `"}]}}`
}
