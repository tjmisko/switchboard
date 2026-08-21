package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestResolveSymptom(t *testing.T) {
	tests := []struct {
		name     string
		flag     string
		words    []string
		wantName string
	}{
		{"explicit flag wins", "green", []string{"red", "stuck"}, "false-green"},
		{"infers stale-red from words", "", []string{"red", "was", "stuck", "for", "ages"}, "stale-red"},
		{"infers false-orange from teammate wording", "", []string{"orange", "but", "teammate", "working"}, "false-orange"},
		{"infers false-green", "", []string{"went", "green", "too", "early"}, "false-green"},
		{"no description → all", "", nil, "all"},
		{"ambiguous tie → all", "", []string{"green", "orange"}, "all"},
		{"unknown flag falls through to inference", "bogus", []string{"red", "stuck"}, "stale-red"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveSymptom(tt.flag, tt.words).name; got != tt.wantName {
				t.Errorf("resolveSymptom(%q, %v) = %q, want %q", tt.flag, tt.words, got, tt.wantName)
			}
		})
	}
}

// A realistic stale-RED episode: onset, a hold, then a slow approve-resume exit.
// diagnose must surface the red duration (from age=) and name the governing knob.
func TestRunDiagnoseStaleRed(t *testing.T) {
	lines := []string{
		`2026-06-23T14:30:00+0000 host switchboard[9]: status: pid=4821 session=ce13c0f2 cwd=/p ->permission (agent=claude event=PermissionRequest)`,
		`2026-06-23T14:30:03+0000 host switchboard[9]: status: pid=4821 session=ce13c0f2 permission==permission rule=case12-hold-bare-result reason="prompt still pending" [S=0 pending="AskUserQuestion" age=3s]`,
		`2026-06-23T14:30:34+0000 host switchboard[9]: status: pid=4821 session=ce13c0f2 permission->working rule=case9-approve-resume reason="transcript: turn resumed" [S=0 pending="AskUserQuestion" age=34s]`,
	}
	var buf bytes.Buffer
	runDiagnose(&buf, lines, resolveSymptom("", []string{"red", "stuck"}), "", 0, 200, false)
	out := buf.String()

	for _, want := range []string{
		"stale/stuck RED",
		"session ce13c0f2 (pid 4821)",
		"permission->working",
		"case9-approve-resume",
		"Tuning.ResumeExitStatus", // the knob annotation
		"RED held for: 34s",       // duration recovered from age=
		"case12-hold-bare-result", // the hold is in the timeline + summary
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// false-orange: an orchestrator goes idle then is promoted to delegating; the
// idle→delegating age is the orange-lag, and DelegatingEnabled is the knob.
func TestRunDiagnoseDelegating(t *testing.T) {
	lines := []string{
		`2026-06-23T14:31:00+0000 host sb[9]: status: pid=7 session=abcdef12 cwd=/p working->idle (agent=claude event=Stop)`,
		`2026-06-23T14:31:05+0000 host sb[9]: status: pid=7 session=abcdef12 idle->delegating rule=case5-delegating reason="idle with subagents in flight" [S=2 pending="" age=5s]`,
	}
	var buf bytes.Buffer
	runDiagnose(&buf, lines, resolveSymptom("orange", nil), "", 0, 200, false)
	out := buf.String()

	for _, want := range []string{"idle->delegating", "case5-delegating", "Tuning.DelegatingEnabled", "S=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRunDiagnoseSessionFilterAndEmpty(t *testing.T) {
	lines := []string{
		`status: pid=1 session=aaaa working->idle (agent=claude event=Stop)`,
		`status: pid=2 session=bbbb working->idle (agent=claude event=Stop)`,
	}
	var buf bytes.Buffer
	runDiagnose(&buf, lines, symAll, "aaaa", 0, 200, false)
	out := buf.String()
	if !strings.Contains(out, "session aaaa") || strings.Contains(out, "session bbbb") {
		t.Errorf("session filter not applied:\n%s", out)
	}

	buf.Reset()
	runDiagnose(&buf, lines, symAll, "nomatch", 0, 200, false)
	if !strings.Contains(buf.String(), "no status-decision lines matched") {
		t.Errorf("empty result should print guidance:\n%s", buf.String())
	}
}

func TestParseAround(t *testing.T) {
	if _, err := parseAround("2026-06-23 14:30:00"); err != nil {
		t.Errorf("full datetime should parse: %v", err)
	}
	if got, err := parseAround("14:30"); err != nil {
		t.Errorf("clock time should parse: %v", err)
	} else if got.Hour() != 14 || got.Minute() != 30 {
		t.Errorf("clock time = %v, want 14:30 today", got)
	}
	if _, err := parseAround("yesterday-ish"); err == nil {
		t.Error("garbage should error with guidance")
	}
}

func TestExtractTime(t *testing.T) {
	if tm, ok := extractTime(`2026-06-23T14:30:01+0000 host unit: status: pid=1 ...`); !ok || tm.IsZero() {
		t.Errorf("short-iso prefix not parsed: ok=%v t=%v", ok, tm)
	}
	if _, ok := extractTime(`status: pid=1 session=x working->idle (agent=claude event=Stop)`); ok {
		t.Error("a line with no timestamp prefix should report ok=false")
	}
}

func TestBuildObserverDiagnosticsContentFreeStatesAndShadowMismatch(t *testing.T) {
	now := time.Date(2026, 8, 21, 18, 0, 0, 0, time.UTC)
	snap := state.Snapshot{Sessions: []state.Session{
		{
			PID: 12, Agent: state.AgentKindCodex, CWD: "/secret/project",
			Codex: &state.AgentInfo{SessionID: "codex-root", Status: state.StatusPermission},
			AgentGraph: &state.AgentGraph{
				RootID: "codex-root", Source: agentgraph.SourceCodexAppServer,
				ObservedAt: now.Add(-time.Second), FreshUntil: now.Add(time.Second), Complete: false,
				Summary: state.AgentGraphSummary{Status: state.StatusPermission},
				Nodes: []state.AgentNode{
					{ID: "codex-root", Nickname: "private root", Runtime: agentgraph.RuntimeIdle, Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleRunning},
					{ID: "child", Description: "private prompt", Runtime: agentgraph.RuntimeSystemError, Attention: agentgraph.AttentionApproval, Lifecycle: agentgraph.LifecycleRunning},
				},
			},
		},
		{PID: 13, Agent: state.AgentKindCodex},
		{
			PID: 14, Agent: state.AgentKindClaude,
			Claude: &state.AgentInfo{SessionID: "claude-root", Status: state.StatusIdle},
			AgentGraph: &state.AgentGraph{
				RootID: "claude-root", Source: agentgraph.SourceClaudeTranscript,
				ObservedAt: now.Add(-time.Minute), FreshUntil: now.Add(-time.Second), Complete: true,
				Summary: state.AgentGraphSummary{Status: state.StatusDelegating},
				Nodes:   []state.AgentNode{{ID: "claude-root", Runtime: agentgraph.RuntimeIdle, Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleRunning}},
			},
		},
	}}

	got := buildObserverDiagnostics(snap, now)
	if len(got) != 3 {
		t.Fatalf("diagnostics = %+v", got)
	}
	if !got[0].Bound || got[0].Freshness != "fresh" || got[0].Snapshot != "partial" || got[0].LiveNodes != 2 || got[0].WaitingNodes != 1 || got[0].ErrorNodes != 1 {
		t.Fatalf("fresh codex diagnostic = %+v", got[0])
	}
	if got[1].Bound || got[1].Freshness != "absent" {
		t.Fatalf("unbound codex diagnostic = %+v", got[1])
	}
	if got[2].Freshness != "expired" || got[2].ShadowMismatch == nil || !*got[2].ShadowMismatch {
		t.Fatalf("stale shadow diagnostic = %+v", got[2])
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"/secret/project", "private root", "private prompt"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, b)
		}
	}
}

func TestRenderObserverDiagnosticsBoundUnboundAndUnknownErrors(t *testing.T) {
	diagnostics := []observerDiagnostic{
		{PID: 1, Provider: "codex", Bound: true, Freshness: "fresh", Snapshot: "complete", Source: "codex_app_server", LiveNodes: 2, WaitingNodes: 1},
		{PID: 2, Provider: "codex", Bound: false, Freshness: "absent", Snapshot: "absent", LastErrorCategory: "unknown"},
	}
	var buf bytes.Buffer
	renderObserverDiagnostics(&buf, diagnostics)
	out := buf.String()
	for _, want := range []string{"pid 1 codex: bound", "fresh", "complete", "live=2 waiting=1", "pid 2 codex: unbound", "error=unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cwd") {
		t.Fatalf("unbound guidance must not recommend CWD correlation:\n%s", out)
	}
}

func TestDiagnoseObserverReadsSnapshotWithoutJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	logPath := filepath.Join(dir, "observer.log")
	b, err := json.Marshal(state.Snapshot{Sessions: []state.Session{{PID: 9, Agent: state.AgentKindCodex}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { cmdDiagnose([]string{"--observer", "--state", path, "--file", logPath, "--json"}) })
	var got []observerDiagnostic
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal observer output: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].PID != 9 || got[0].Bound {
		t.Fatalf("diagnostics = %+v", got)
	}
}

func TestParseObserverLogRecordsIsStrictAndContentFree(t *testing.T) {
	lines := []string{
		`2026-08-21T18:00:01+0000 host switchboard[9]: agent-observer: provider=codex category=observe_error count=3 raw="secret prompt"`,
		`2026-08-21T18:00:02+0000 host switchboard[9]: agent-observer: provider=codex category=not-safe! count=4`,
		`agent-observer: provider=other category=observe_error count=5`,
	}
	got := parseObserverLogRecords(lines)
	if len(got) != 1 || got[0].Provider != "codex" || got[0].Category != "observe_error" || got[0].Count != 3 {
		t.Fatalf("records = %+v", got)
	}
	diagnostics := []observerDiagnostic{{Provider: "codex", ObserverConnection: "not_reported", LastErrorCategory: "not_reported"}}
	applyObserverLogDiagnostics(diagnostics, got)
	if diagnostics[0].ObserverConnection != "error" || diagnostics[0].LastErrorCategory != "observe_error" || diagnostics[0].LastErrorCount != 3 {
		t.Fatalf("merged = %+v", diagnostics[0])
	}
	b, err := json.Marshal(diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret prompt") || strings.Contains(string(b), "raw") {
		t.Fatalf("diagnostic retained raw content: %s", b)
	}
}
