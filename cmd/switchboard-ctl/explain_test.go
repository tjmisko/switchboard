package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/statusexplain"
)

func sampleExplanation() statusexplain.Decision {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	d := statusexplain.Decision{
		Root: statusexplain.Root{PID: 4242, StartedAt: at.Add(-time.Hour), Provider: "codex", SessionID: "thread-1"},
		Choice: statusexplain.Choice{Status: "permission", Source: "codex_app_server",
			EvidenceKind: statusexplain.EvidenceProviderSnapshot, Reason: statusexplain.ReasonHerdrYieldAttention,
			ObservedAt: at, FreshUntil: at.Add(time.Minute), DecidedAt: at.Add(time.Second)},
	}
	d.AddRejected(statusexplain.Candidate{Source: "herdr", EvidenceKind: statusexplain.EvidenceTerminal,
		Status: "working", ObservedAt: at.Add(-time.Minute), RejectReason: statusexplain.ReasonHerdrYieldAttention})
	return d
}

// Acceptance criterion 4, at the CLI: both forms render one record and agree.
func TestRenderExplanationShouldPrintTheSameRecordWhenTextOrJSONIsAsked(t *testing.T) {
	var text, raw bytes.Buffer
	renderExplanation(&text, sampleExplanation(), false)
	renderExplanation(&raw, sampleExplanation(), true)

	var decoded statusexplain.Decision
	if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
		t.Fatalf("json output does not decode: %v\n%s", err, raw.String())
	}
	var again bytes.Buffer
	renderExplanation(&again, decoded, false)
	if again.String() != text.String() {
		t.Fatalf("text and JSON disagree:\n%s\nvs\n%s", text.String(), again.String())
	}
	for _, want := range []string{"pid=4242", "status=permission", "reason=herdr_yield_attention",
		"rejected status=working source=herdr", "reject_reason=herdr_yield_attention"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text lacks %q:\n%s", want, text.String())
		}
	}
}

func TestRenderExplanationShouldSanitizeWhenADaemonSendsContent(t *testing.T) {
	d := sampleExplanation()
	d.Reason = "the user asked me to run rm -rf"
	d.Root.SessionID = "a prompt with spaces"
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		renderExplanation(&out, d, asJSON)
		if strings.Contains(out.String(), "rm -rf") || strings.Contains(out.String(), "prompt with") {
			t.Fatalf("json=%v printed content:\n%s", asJSON, out.String())
		}
	}
}

func TestRunDiagnoseShouldPointAtExplainWhenRenderingText(t *testing.T) {
	var out bytes.Buffer
	runDiagnose(&out, nil, symptom{headline: "test"}, "", 0, 10, false)
	if !strings.Contains(out.String(), "switchboard-ctl explain --pid") {
		t.Fatalf("diagnose output lacks the explain pointer:\n%s", out.String())
	}
}
