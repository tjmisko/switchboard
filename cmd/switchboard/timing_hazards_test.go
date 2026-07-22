package main

import (
	"os"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// A running catalog of tricky timing situations in the hookless status recovery,
// each pinned as a regression. Status hooks (UserPromptSubmit, Stop, PostToolUse,
// …) and the transcript are two separate event streams the reconciler has to
// reconcile, and a fast user action that lands in the gap between a hook and the
// next exposes clock-ordering races. (H9 adds a third stream — the pane title —
// for the one transition that leaves both silent.) Every row is a scenario that
// bit us — or plausibly could — kept executable so a regression flips a chip
// color and fails the build.
//
// Each row models the real two-phase flow:
//
//  1. a status hook fires; the daemon dates the transition via the anchoring
//     policy (transcript.AnchorSince): an edge into working anchors to the newest
//     transcript entry at that instant (the skew fix), an edge into idle or
//     permission anchors to wall-clock now (the flush-race fix);
//  2. the transcript keeps growing (a later interrupt, a teammate's tool_result,
//     a `!bash` line, a turn's final assistant message flushed after its Stop, …);
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
		hookAt    string   // wall-clock instant the daemon ran the hook (default: newest atHook entry; set later to model a flush lag)
		subagents int      // teammates in flight at reconcile
		title     string   // pane title at reconcile (the agent CLI's status glyph); "" = no title signal
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
			// THE bug this branch fixes (the flush-ordering race). A turn ends: its
			// final assistant message (t30s) is generated, the Stop hook fires, and the
			// daemon processes it at hookAt (t40s) — but Claude flushes that assistant
			// line to the .jsonl a beat LATER, so at hook time the newest entry on disk
			// is only the earlier tool_result (t0). Anchoring StatusSince to that stale
			// on-disk entry let the late-flushed assistant message read as "activity
			// after idle" and re-green the chip after EVERY Stop. Anchoring an idle edge
			// to wall-clock now (hookAt, after the turn truly ended) keeps it idle: the
			// completing turn's own message (t30s, before t40s) cannot re-trigger.
			id:        "H7-stop-final-message-flush-race",
			atHook:    []string{tResult(t0)},
			status:    "idle",
			hookAt:    t40s,
			appended:  []string{tAssistant(t30s)},
			afterHook: 5 * time.Minute,
			want:      "idle",
		},
		{
			// The zettel false-green (2026-07-22, session f4aff00a): the same
			// flush-ordering race as H7, on the INTO-PERMISSION edge — where the cost
			// is missed-RED, the worst error in the §4 ranking. A turn's pre-prompt
			// thinking/text (t30s) and the AskUserQuestion tool_use (t40s) are
			// generated before the PermissionRequest hook but flushed to the .jsonl a
			// beat AFTER the daemon processes it, so at hook time the newest entry on
			// disk is an earlier tool_result (t0). Anchoring StatusSince to that stale
			// entry let the blocked turn's OWN late-flushed assistant entries read as
			// "assistant message after since → turn resumed" on the next tick, exiting
			// red to green while the prompt sat unanswered for 7½ more minutes. A
			// permission edge must anchor to wall-clock now, like idle: every entry of
			// the prompt's own turn is dated at-or-before hook processing and cannot
			// prove resolution, while a genuine post-approval assistant message —
			// generated after the user acted — still can.
			id:        "H8-permission-preprompt-flush-race",
			atHook:    []string{tResult(t0)},
			status:    "permission",
			hookAt:    t40s,
			appended:  []string{tAssistant(t30s), tAskToolUse(t40s)},
			afterHook: 5 * time.Minute,
			want:      "permission",
		},
		{
			// The silent abort (2026-07-22, session be0d8122): a prompt is submitted
			// and interrupted (double-Esc) before the first token streams. No Stop
			// hook fires (interrupts never do) AND no interrupt marker is written
			// (there was no in-flight response to mark), so BOTH event streams are
			// silent forever and the chip would stay green until the next manual
			// prompt. The recovery is a third stream: the pane title, where Claude
			// Code parks the static idle glyph (✳) while waiting at the prompt and
			// animates a spinner while a turn runs. A fresh idle-glyph sighting
			// (TitleAt after the working edge) past the grace window demotes to idle.
			id:        "H9-instant-interrupt-silent-abort",
			atHook:    []string{tUserText(t0, "how should this work on mobile?")},
			status:    "working",
			title:     "✳ align-project-messaging",
			afterHook: 5 * time.Minute,
			want:      "idle",
		},
		{
			// Contrast: the same quiet transcript mid-turn (a slow first inference,
			// H3's cousin) shows a SPINNER title — the demotion keys on the idle
			// glyph specifically, never on transcript silence.
			id:        "H9-spinner-title-holds-green",
			atHook:    []string{tUserText(t0, "how should this work on mobile?")},
			status:    "working",
			title:     "⠐ align-project-messaging",
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
			// Phase 1: the daemon processes the hook at wall-clock `hookNow`, then
			// dates StatusSince per its anchoring policy (transcript.AnchorSince) over
			// the transcript visible AT THAT INSTANT. hookNow defaults to the newest
			// atHook entry (no flush lag); a flush-race row overrides it to a time after
			// an entry that has not been flushed yet.
			atHookPath := writeTranscript(t, h.atHook...)
			anchor, ok := transcript.AnchorTime(atHookPath, transcript.DefaultTailBytes)
			if !ok {
				t.Fatalf("AnchorTime: no turn entry in atHook fixture")
			}
			hookNow := anchor
			if h.hookAt != "" {
				hookNow = mustParseTime(t, h.hookAt)
			}
			since := transcript.AnchorSince(atHookPath, hookNow, h.status == "working", transcript.DefaultTailBytes)
			reconcileNow := hookNow.Add(h.afterHook)

			// Phase 2: the transcript grows to its reconcile-time tail.
			full := writeTranscript(t, append(append([]string{}, h.atHook...), h.appended...)...)
			mtime := reconcileNow
			if h.stale {
				mtime = since.Add(-time.Hour)
			}
			if err := os.Chtimes(full, mtime, mtime); err != nil {
				t.Fatalf("chtimes: %v", err)
			}

			// Phase 3: a reconcile tick must land on the right color. Both self-heals
			// run in reconcileOnce's order: stale-attention releases (or holds) a
			// permission latch, stuck-status recovers the working/idle latches.
			m := stuckMap(h.status, full, since)
			m[100].Claude.InFlightSubagents = h.subagents
			if h.status == "permission" {
				m[100].Claude.PendingTool = "AskUserQuestion"
			}
			if h.title != "" {
				// The resolver refreshed the pane title on this tick (H9): the title
				// was sampled after the chip's transition, as a live locate would be.
				m[100].Agent = state.AgentKindClaude
				m[100].Wezterm = &state.WeztermInfo{Title: h.title, TitleAt: reconcileNow.Add(-time.Second)}
			}
			selfHealStaleAttention(m, reconcileNow, testTune, nil)
			selfHealStuckStatus(m, reconcileNow, testTune, nil)

			if got := m[100].Claude.Status; got != h.want {
				t.Errorf("status = %q, want %q", got, h.want)
			}
		})
	}
}

// TestIdleTitleDemotion pins every guard on the H9 idle-title demotion: the
// rule must fire only on positive, fresh evidence — a claude session, a title
// sampled after the chip went working, the idle glyph itself, and a chip old
// enough to be past the edge lag. Anything less holds the current color, so a
// terminal without agent titles (or a disabled knob) exactly preserves the old
// behavior.
func TestIdleTitleDemotion(t *testing.T) {
	const t0 = "2026-06-22T10:50:00Z"
	cases := []struct {
		name      string
		agent     string
		status    string
		title     string
		titleAgo  time.Duration // how long before the reconcile the title was sampled; negative = before the working edge
		age       time.Duration // how long the chip has held its status
		suspended bool
		noWezterm bool
		disabled  bool
		want      string
	}{
		{name: "should demote a working chip when a fresh idle-glyph title outlives the grace",
			agent: "claude", status: "working", title: "✳ my-session", titleAgo: time.Second, age: time.Minute, want: "idle"},
		{name: "should hold when the title was sampled before the working edge",
			agent: "claude", status: "working", title: "✳ my-session", titleAgo: 2 * time.Minute, age: time.Minute, want: "working"},
		{name: "should hold inside the grace window",
			agent: "claude", status: "working", title: "✳ my-session", titleAgo: time.Second, age: 5 * time.Second, want: "working"},
		{name: "should hold for a codex session",
			agent: "codex", status: "working", title: "✳ my-session", titleAgo: time.Second, age: time.Minute, want: "working"},
		{name: "should hold while suspended",
			agent: "claude", status: "working", title: "✳ my-session", titleAgo: time.Second, age: time.Minute, suspended: true, want: "working"},
		{name: "should hold with no terminal mapping",
			agent: "claude", status: "working", noWezterm: true, age: time.Minute, want: "working"},
		{name: "should hold when the knob is off",
			agent: "claude", status: "working", title: "✳ my-session", titleAgo: time.Second, age: time.Minute, disabled: true, want: "working"},
		{name: "should not touch an idle chip showing the idle glyph",
			agent: "claude", status: "idle", title: "✳ my-session", titleAgo: time.Second, age: time.Minute, want: "idle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTranscript(t, tUserText(t0, "do the thing"))
			since := mustParseTime(t, t0)
			now := since.Add(tc.age)
			m := stuckMap(tc.status, path, since)
			m[100].Agent = tc.agent
			m[100].Suspended = tc.suspended
			if !tc.noWezterm {
				m[100].Wezterm = &state.WeztermInfo{Title: tc.title, TitleAt: now.Add(-tc.titleAgo)}
			}
			tun := testTune
			tun.IdleTitleDemotionEnabled = !tc.disabled
			selfHealStuckStatus(m, now, tun, nil)
			if got := m[100].Claude.Status; got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// tUserText is a user prompt entry — the transcript record Claude Code writes
// (and dates) just before firing UserPromptSubmit.
func tUserText(ts, s string) string {
	return `{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":[{"type":"text","text":"` + s + `"}]}}`
}

// tAskToolUse is the assistant entry carrying the AskUserQuestion tool_use — the
// record whose creation fires the PermissionRequest hook. It is an assistant
// entry, so if it (or any same-turn sibling) lands after StatusSince it reads as
// resolution — the H8 hazard.
func tAskToolUse(ts string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ask","name":"AskUserQuestion","input":{}}]}}`
}

// mustParseTime parses an RFC3339 fixture timestamp into the wall-clock instant a
// row uses for hookAt (the moment the daemon ran the hook).
func mustParseTime(t *testing.T, ts string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("parse %q: %v", ts, err)
	}
	return parsed
}
