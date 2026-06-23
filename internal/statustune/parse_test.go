package statustune

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// logged captures what d.Log() writes (including the Go `log` date prefix), so the
// round-trip test proves ParseDecision is the exact inverse of Decision.Log over
// a real log line — prefix and all.
func logged(d Decision) string {
	var buf bytes.Buffer
	out := log.Writer()
	defer log.SetOutput(out)
	log.SetOutput(&buf)
	d.Log()
	return strings.TrimRight(buf.String(), "\n")
}

func TestParseDecisionRoundTrip(t *testing.T) {
	cases := []Decision{
		{PID: 100, Session: "ce13c0f2", From: "permission", To: "working", Rule: RuleApproveResume, Reason: "transcript: turn resumed", Subagents: 2, Pending: "AskUserQuestion", Age: 27 * time.Second},
		{PID: 4821, Session: "abcdef12", From: "idle", To: "delegating", Rule: RuleDelegating, Reason: "idle with subagents in flight", Subagents: 1, Pending: "", Age: 5 * time.Second},
		{PID: 7, Session: "0199736b", From: "permission", To: "permission", Rule: RuleHoldBareResult, Reason: `tool-name mismatch: "Task"`, Subagents: 0, Pending: "AskUserQuestion", Age: 3 * time.Second},
	}
	for _, d := range cases {
		t.Run(d.Rule, func(t *testing.T) {
			rec, ok := ParseDecision(logged(d))
			if !ok {
				t.Fatalf("ParseDecision returned ok=false for %q", logged(d))
			}
			if rec.PID != d.PID || rec.Session != d.Session || rec.From != d.From || rec.To != d.To {
				t.Errorf("identity mismatch: got pid=%d session=%s %s->%s", rec.PID, rec.Session, rec.From, rec.To)
			}
			if rec.Rule != d.Rule || rec.Reason != d.Reason {
				t.Errorf("rule/reason: got rule=%q reason=%q, want %q / %q", rec.Rule, rec.Reason, d.Rule, d.Reason)
			}
			if rec.Subagents != d.Subagents || rec.Pending != d.Pending || rec.Age != d.Age.Round(time.Second) {
				t.Errorf("tuple: got S=%d pending=%q age=%v", rec.Subagents, rec.Pending, rec.Age)
			}
			if rec.Kind != KindReconciler || !rec.HasTuple {
				t.Errorf("kind/tuple flags: kind=%q hasTuple=%v", rec.Kind, rec.HasTuple)
			}
			if rec.Hold != (d.From == d.To) {
				t.Errorf("Hold = %v, want %v", rec.Hold, d.From == d.To)
			}
		})
	}
}

// The hook-edge format (emitted by rpc, with a window-title or cwd label between
// the session id and the transition, and a possibly-empty FROM) must also parse.
func TestParseDecisionHookEdges(t *testing.T) {
	tests := []struct {
		name             string
		line             string
		wantFrom, wantTo string
		wantEvent        string
	}{
		{
			name:      "cwd label with empty FROM",
			line:      "status: pid=42 session=ce13c0f2 cwd=/home/u/proj ->idle (agent=claude event=Stop)",
			wantFrom:  "",
			wantTo:    "idle",
			wantEvent: "Stop",
		},
		{
			name:      "quoted window-title label",
			line:      `status: pid=42 session=ce13c0f2 "my project" idle->working (agent=claude event=PostToolUse)`,
			wantFrom:  "idle",
			wantTo:    "working",
			wantEvent: "PostToolUse",
		},
		{
			name:      "journalctl prefix is ignored",
			line:      `2026-06-23T14:30:01+0000 host switchboard[123]: status: pid=42 session=ce13c0f2 cwd=/p working->idle (agent=claude event=Stop)`,
			wantFrom:  "working",
			wantTo:    "idle",
			wantEvent: "Stop",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, ok := ParseDecision(tt.line)
			if !ok {
				t.Fatalf("ok=false for %q", tt.line)
			}
			if rec.Kind != KindHook {
				t.Errorf("kind = %q, want hook", rec.Kind)
			}
			if rec.From != tt.wantFrom || rec.To != tt.wantTo {
				t.Errorf("transition = %q->%q, want %q->%q", rec.From, rec.To, tt.wantFrom, tt.wantTo)
			}
			if rec.Event != tt.wantEvent {
				t.Errorf("event = %q, want %q", rec.Event, tt.wantEvent)
			}
		})
	}
}

func TestParseDecisionRejectsNonDecisionLines(t *testing.T) {
	for _, line := range []string{
		"",
		"backends: wm=hyprland terminal=wezterm observe=true navigate=true",
		"switchboard listening on /run/user/1000/switchboard.sock",
		"status: pid=notanumber session=x working->idle (agent=claude event=Stop)",
	} {
		if _, ok := ParseDecision(line); ok {
			t.Errorf("ParseDecision(%q) = ok, want not-ok", line)
		}
	}
}

// Every rule the daemon can emit must have a concrete knob hint (Field set, or a
// deliberate "no knob" explanation), so `diagnose` can always answer "what do I
// change?".
func TestRuleKnobCoverage(t *testing.T) {
	all := []string{
		RuleApproveToolMatch, RuleApproveTranscript, RuleHoldBareResult,
		RuleApproveResume, RuleDeclineIdle, RuleDeclineDelegating, RuleTTLBackstop,
		RuleDelegating, RuleDrained, RuleResumeActivity, RuleInterrupt,
	}
	for _, r := range all {
		h := RuleKnob(r)
		if h.What == "" {
			t.Errorf("rule %q has no knob hint", r)
		}
	}
	if got := RuleKnob("bogus-rule"); got.Field != "" || got.What == "" {
		t.Errorf("unknown rule should yield an empty Field with an explanatory What, got %+v", got)
	}
}
