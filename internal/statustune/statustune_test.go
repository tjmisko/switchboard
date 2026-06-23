package statustune

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// Default must encode the recommended §8 answers; these are the knobs other
// packages read, so pin them so an accidental flip is caught.
func TestDefault(t *testing.T) {
	d := Default()
	if d.PermissionDecayTTL != 30*time.Second {
		t.Errorf("PermissionDecayTTL = %v, want 30s", d.PermissionDecayTTL)
	}
	if d.TailBytes != 128*1024 {
		t.Errorf("TailBytes = %d, want 131072", d.TailBytes)
	}
	if !d.EarlyClearApproveByToolName {
		t.Error("EarlyClearApproveByToolName = false, want true (Q2)")
	}
	if d.ResumeExitStatus != "working" {
		t.Errorf("ResumeExitStatus = %q, want working (P3)", d.ResumeExitStatus)
	}
	if d.InterruptExitStatus != "idle" {
		t.Errorf("InterruptExitStatus = %q, want idle (P3)", d.InterruptExitStatus)
	}
	if !d.DelegatingEnabled {
		t.Error("DelegatingEnabled = false, want true (P4)")
	}
	if d.EscWithTeammatesStatus != "delegating" {
		t.Errorf("EscWithTeammatesStatus = %q, want delegating (Q3)", d.EscWithTeammatesStatus)
	}
}

func TestDecisionLog(t *testing.T) {
	var buf bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&buf)

	t.Run("a transition logs from->to with the full tuple", func(t *testing.T) {
		buf.Reset()
		Decision{
			PID: 100, Session: "ce13c0f2", From: "permission", To: "working",
			Rule: "case9-approve-resume", Reason: "tool-name match: AskUserQuestion",
			Subagents: 2, Pending: "AskUserQuestion", Age: 27 * time.Second,
		}.Log()
		out := buf.String()
		want := `status: pid=100 session=ce13c0f2 permission->working rule=case9-approve-resume reason="tool-name match: AskUserQuestion" [S=2 pending="AskUserQuestion" age=27s]`
		if !strings.Contains(out, want) {
			t.Errorf("log line\n  got:  %s\n  want substring: %s", strings.TrimSpace(out), want)
		}
	})

	t.Run("a hold logs with == instead of -> so it is distinguishable", func(t *testing.T) {
		buf.Reset()
		Decision{
			PID: 100, Session: "ce13c0f2", From: "permission", To: "permission",
			Rule: "case12-hold-bare-result", Reason: "prompt still pending",
		}.Log()
		if !strings.Contains(buf.String(), "permission==permission rule=case12-hold-bare-result") {
			t.Errorf("hold line missing == marker:\n%s", buf.String())
		}
	})
}
