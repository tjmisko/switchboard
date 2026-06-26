package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
)

// TestTimelineEndToEnd drives a full fake session — start, name, focus, work,
// per-model usage, a subagent, an idle/active activity edge, end — through the
// real asynchronous Sink (the daemon's writer) onto disk as JSONL, then renders
// it back through `timeline --json` and asserts the whole v2 contract: the
// recorded JSONL, the per-lane label/subagent/focus/cost enrichments, the
// recomputed totals, the delegation metrics, the rolling plan_window, and the
// top-level activity timeline the dashboard overlays.
func TestTimelineEndToEnd(t *testing.T) {
	dir := t.TempDir()

	// Recent, so the session lands in both today's window and the rolling 5h
	// plan window. A 10-minute span gives the delegation math room to split.
	now := time.Now().UTC()
	t0 := now.Add(-12 * time.Minute)
	day := t0.Format("2006-01-02")
	at := func(min, sec int) time.Time {
		return t0.Add(time.Duration(min)*time.Minute + time.Duration(sec)*time.Second)
	}

	// Full-detail sink so the session name and subagent description survive.
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: dir})
	events := []history.Event{
		{Ts: at(0, 0), Type: history.EventSessionStart, PID: 1, SessionID: "s1", Agent: "claude", Project: "demo"},
		{Ts: at(0, 0), Type: history.EventSessionLabel, PID: 1, SessionID: "s1", Label: "delegation-demo"},
		{Ts: at(0, 0), Type: history.EventFocus, PID: 1, SessionID: "s1"}, // user focuses s1
		{Ts: at(0, 0), Type: history.EventTransition, PID: 1, SessionID: "s1", To: "working"},
		{Ts: at(1, 0), Type: history.EventUsageSample, PID: 1, SessionID: "s1", Model: "claude-opus-4-8", TokIn: 1_000_000, TokOut: 100_000}, // $7.50
		{Ts: at(1, 0), Type: history.EventUsageSample, PID: 1, SessionID: "s1", Model: "claude-sonnet-4-6", TokIn: 1_000_000},                // $3.00
		{Ts: at(2, 0), Type: history.EventSubagentSpawn, PID: 1, SessionID: "s1", ToolUseID: "tu1", AgentType: "Explore", Description: "scan the repo"},
		{Ts: at(3, 0), Type: history.EventActivity, To: "idle"}, // user steps away
		{Ts: at(5, 0), Type: history.EventSubagentStop, PID: 1, SessionID: "s1", ToolUseID: "tu1"},
		{Ts: at(9, 0), Type: history.EventActivity, To: "active"}, // user returns
		{Ts: at(10, 0), Type: history.EventTransition, PID: 1, SessionID: "s1", To: "idle"},
		{Ts: at(10, 0), Type: history.EventSessionEnd, PID: 1, SessionID: "s1"},
	}
	for _, ev := range events {
		sink.Record(ev)
	}
	sink.Close() // flushes the writer goroutine

	// --- assert the JSONL the writer produced ---
	raw, err := os.ReadFile(history.DayPath(dir, day))
	if err != nil {
		t.Fatalf("read day-file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != len(events) {
		t.Fatalf("recorded %d JSONL lines, want %d", len(lines), len(events))
	}
	seen := map[string]bool{}
	for _, ln := range lines {
		var ev history.Event
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("malformed JSONL line %q: %v", ln, err)
		}
		seen[ev.Type] = true
	}
	for _, want := range []string{
		history.EventSessionStart, history.EventSessionLabel, history.EventFocus,
		history.EventTransition, history.EventUsageSample, history.EventSubagentSpawn,
		history.EventActivity, history.EventSubagentStop, history.EventSessionEnd,
	} {
		if !seen[want] {
			t.Errorf("JSONL missing event type %q", want)
		}
	}

	// --- render it back through timeline --json and assert the v2 shape ---
	out := captureStdout(t, func() {
		cmdTimeline([]string{"--dir", dir, "--day", day, "--json", "--plan-window"})
	})
	var env struct {
		Lanes []struct {
			Labels    []history.LabelSpan    `json:"labels"`
			Subagents []history.SubagentSpan `json:"subagents"`
			Focus     []history.FocusSpan    `json:"focus"`
			CostUSD   float64                `json:"cost_usd"`
			TokIn     int64                  `json:"tok_in"`
		} `json:"lanes"`
		Summary    history.Summary        `json:"summary"`
		Totals     history.Totals         `json:"totals"`
		Activity   []history.ActivitySpan `json:"activity"`
		PlanWindow *history.PlanWindow    `json:"plan_window"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, out)
	}

	if len(env.Lanes) != 1 {
		t.Fatalf("lanes = %d, want 1\n%s", len(env.Lanes), out)
	}
	lane := env.Lanes[0]
	if len(lane.Labels) == 0 || lane.Labels[0].Label != "delegation-demo" {
		t.Errorf("lane.labels = %+v, want a delegation-demo span", lane.Labels)
	}
	if len(lane.Subagents) == 0 || lane.Subagents[0].ToolUseID != "tu1" ||
		lane.Subagents[0].AgentType != "Explore" || lane.Subagents[0].Description != "scan the repo" {
		t.Errorf("lane.subagents = %+v, want the tu1/Explore span", lane.Subagents)
	}
	if len(lane.Focus) == 0 {
		t.Errorf("lane.focus empty — focus span not derived")
	}
	if !approxF(lane.CostUSD, 10.50) {
		t.Errorf("lane.cost_usd = %v, want ~10.50 (opus 7.50 + sonnet 3.00)", lane.CostUSD)
	}
	if lane.TokIn != 2_000_000 {
		t.Errorf("lane.tok_in = %d, want 2_000_000", lane.TokIn)
	}
	if !approxF(env.Totals.CostUSD, 10.50) {
		t.Errorf("totals.cost_usd = %v, want ~10.50", env.Totals.CostUSD)
	}

	// Delegation math: working [0,10m]; focus [0,10m]; user-active [0,3m]∪[9m,10m]=4m.
	// attended = 4m, delegated = 6m, effectiveness = 0.6, prompt = 4m.
	if env.Summary.DelegatedActive != 6*time.Minute {
		t.Errorf("delegated_active = %v, want 6m", env.Summary.DelegatedActive)
	}
	if env.Summary.AttendedActive != 4*time.Minute {
		t.Errorf("attended_active = %v, want 4m", env.Summary.AttendedActive)
	}
	if env.Summary.PromptActive != 4*time.Minute {
		t.Errorf("prompt_active = %v, want 4m", env.Summary.PromptActive)
	}
	if !approxF(env.Summary.DelegationEffectiveness, 0.6) {
		t.Errorf("delegation_effectiveness = %v, want 0.6", env.Summary.DelegationEffectiveness)
	}

	// Top-level activity timeline: active, idle, active.
	wantStates := []string{"active", "idle", "active"}
	if len(env.Activity) != len(wantStates) {
		t.Fatalf("activity spans = %+v, want %v", env.Activity, wantStates)
	}
	for i, st := range wantStates {
		if env.Activity[i].State != st {
			t.Errorf("activity[%d].state = %q, want %q", i, env.Activity[i].State, st)
		}
	}

	if env.PlanWindow == nil {
		t.Fatalf("plan_window absent with --plan-window\n%s", out)
	}
	if !approxF(env.PlanWindow.CostUSD, 10.50) {
		t.Errorf("plan_window.cost_usd = %v, want ~10.50", env.PlanWindow.CostUSD)
	}
}

func approxF(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
