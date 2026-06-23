package main

import (
	"bytes"
	"strings"
	"testing"
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
