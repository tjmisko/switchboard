package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

// workflowFixture lays out one session's workflow records the way Claude Code
// writes them:
//
//	<dir>/<session>.jsonl                                  (parent transcript)
//	<dir>/<session>/subagents/workflows/<runID>/journal.jsonl
//	<dir>/<session>/workflows/scripts/<scriptStem>.js
//
// and returns the parent transcript path plus the run dir.
func workflowFixture(t *testing.T, runID, scriptStem string, journalLines []string) (transcriptPath, runDir string) {
	t.Helper()
	dir := t.TempDir()
	transcriptPath = filepath.Join(dir, "sess.jsonl")
	writeFile(t, transcriptPath, []string{`{"type":"user"}`})
	runDir = filepath.Join(dir, "sess", "subagents", "workflows", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", runDir, err)
	}
	if len(journalLines) > 0 {
		writeFile(t, filepath.Join(runDir, "journal.jsonl"), journalLines)
	}
	if scriptStem != "" {
		scriptsDir := filepath.Join(dir, "sess", "workflows", "scripts")
		if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", scriptsDir, err)
		}
		writeFile(t, filepath.Join(scriptsDir, scriptStem+".js"), []string{"export const meta = {}"})
	}
	return transcriptPath, runDir
}

func journalStarted(agentID string) string {
	return `{"type":"started","key":"v2:abc","agentId":"` + agentID + `"}`
}

func journalResult(agentID string) string {
	return `{"type":"result","key":"v2:abc","agentId":"` + agentID + `","result":{"big":"payload"}}`
}

func TestWorkflowRunsForTranscriptShouldReturnNilWhenNoWorkflowEverRan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	writeFile(t, path, []string{`{"type":"user"}`})
	runs, err := WorkflowRunsForTranscript(path)
	if err != nil {
		t.Fatalf("WorkflowRunsForTranscript: %v", err)
	}
	if runs != nil {
		t.Fatalf("runs = %v, want nil for a session with no workflows dir", runs)
	}
}

func TestWorkflowRunsForTranscriptShouldResolveNameFromScriptFilenameWhenPresent(t *testing.T) {
	path, runDir := workflowFixture(t, "wf_5e3cb808-2ac", "simplification-audit-wf_5e3cb808-2ac",
		[]string{journalStarted("a1")})
	runs, err := WorkflowRunsForTranscript(path)
	if err != nil {
		t.Fatalf("WorkflowRunsForTranscript: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.RunID != "wf_5e3cb808-2ac" {
		t.Errorf("RunID = %q", r.RunID)
	}
	if r.Name != "simplification-audit" {
		t.Errorf("Name = %q, want simplification-audit", r.Name)
	}
	if r.Dir != runDir {
		t.Errorf("Dir = %q, want %q", r.Dir, runDir)
	}
	if r.Journal != filepath.Join(runDir, "journal.jsonl") {
		t.Errorf("Journal = %q", r.Journal)
	}
}

func TestWorkflowRunsForTranscriptShouldKeepEmptyNameWhenScriptIsMissing(t *testing.T) {
	path, _ := workflowFixture(t, "wf_deadbeef-123", "", []string{journalStarted("a1")})
	runs, err := WorkflowRunsForTranscript(path)
	if err != nil {
		t.Fatalf("WorkflowRunsForTranscript: %v", err)
	}
	if len(runs) != 1 || runs[0].Name != "" {
		t.Fatalf("runs = %+v, want one run with empty Name", runs)
	}
}

// A workflow name may itself contain "-wf_" lookalikes; only the LAST "-wf_"
// boundary splits name from run id, so the whole prefix survives as the name.
func TestWorkflowRunsForTranscriptShouldSplitScriptNameOnLastRunIDBoundary(t *testing.T) {
	path, _ := workflowFixture(t, "wf_22-bbb", "fix-wf_journal-parse-wf_22-bbb",
		[]string{journalStarted("a1")})
	runs, err := WorkflowRunsForTranscript(path)
	if err != nil {
		t.Fatalf("WorkflowRunsForTranscript: %v", err)
	}
	if len(runs) != 1 || runs[0].Name != "fix-wf_journal-parse" {
		t.Fatalf("runs = %+v, want Name fix-wf_journal-parse", runs)
	}
}

func TestWorkflowJournalSinceShouldReportStartedAndResultedAcrossOffsets(t *testing.T) {
	_, runDir := workflowFixture(t, "wf_1", "", []string{
		journalStarted("a1"), journalStarted("a2"), journalResult("a1"),
	})
	journal := filepath.Join(runDir, "journal.jsonl")

	started, resulted, off, err := WorkflowJournalSince(journal, 0)
	if err != nil {
		t.Fatalf("WorkflowJournalSince: %v", err)
	}
	if len(started) != 2 || started[0] != "a1" || started[1] != "a2" {
		t.Errorf("started = %v, want [a1 a2]", started)
	}
	if len(resulted) != 1 || resulted[0] != "a1" {
		t.Errorf("resulted = %v, want [a1]", resulted)
	}

	// Appending after the cursor reports only the delta.
	appendFile(t, journal, []string{journalResult("a2"), journalStarted("a3")})
	started, resulted, _, err = WorkflowJournalSince(journal, off)
	if err != nil {
		t.Fatalf("WorkflowJournalSince delta: %v", err)
	}
	if len(started) != 1 || started[0] != "a3" {
		t.Errorf("delta started = %v, want [a3]", started)
	}
	if len(resulted) != 1 || resulted[0] != "a2" {
		t.Errorf("delta resulted = %v, want [a2]", resulted)
	}
}

func TestWorkflowJournalSinceShouldErrWhenJournalDoesNotExistYet(t *testing.T) {
	_, _, _, err := WorkflowJournalSince(filepath.Join(t.TempDir(), "journal.jsonl"), 0)
	if err == nil {
		t.Fatal("want error for a journal that does not exist yet")
	}
}

func TestWorkflowAgentTranscriptShouldRejectPathEscapes(t *testing.T) {
	w := WorkflowRun{Dir: "/tmp/wf_x"}
	if got := w.AgentTranscript("../../etc/passwd"); got != "" {
		t.Errorf("AgentTranscript(escape) = %q, want empty", got)
	}
	if got := w.AgentTranscript(""); got != "" {
		t.Errorf("AgentTranscript(empty) = %q, want empty", got)
	}
	if got := w.AgentTranscript("a1"); got != filepath.Join("/tmp/wf_x", "agent-a1.jsonl") {
		t.Errorf("AgentTranscript(a1) = %q", got)
	}
}
