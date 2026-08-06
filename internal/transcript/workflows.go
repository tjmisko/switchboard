package transcript

// This file recovers ultracode Workflow runs — the fan-outs the Workflow tool
// orchestrates — from the records Claude Code writes per session. A workflow's
// subagents do NOT live in the flat subagents/ dir that SubagentsForTranscript
// scans, and they fire no hooks the daemon can see: while one runs, the parent
// session's main thread is idle (its Stop hook already fired) and every
// observable signal lives under a per-run directory:
//
//	<dir>/<session-id>/subagents/workflows/wf_<runid>/
//	    journal.jsonl            append-only run journal (see WorkflowJournalSince)
//	    agent-<id>.jsonl         each subagent's own transcript (mtime = activity)
//	    agent-<id>.meta.json     always {"agentType":"workflow-subagent","spawnDepth":1}
//	<dir>/<session-id>/workflows/scripts/<name>-wf_<runid>.js
//	                             the persisted script; its filename carries the
//	                             workflow name
//
// The journal is the authoritative per-agent ledger: exactly two line shapes,
// {"type":"started","key":…,"agentId":…} and {"type":"result","key":…,
// "agentId":…,"result":…}. It carries NO timestamps — timing comes from file
// mtimes — and a killed run leaves started-without-result orphans forever, so
// "started and not resulted" alone never proves in-flight; the consumer bounds
// it with the agent transcript's mtime (the fanout Observer's stale cap).
//
// A subagent that returns via structured output ends its transcript with a
// user tool_result line, not an assistant end_turn — so the flat-dir Done
// detection (subagentJSONLState) does NOT transfer; the journal's result event
// is the completion signal for workflow agents.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Directory/file names bracketing a workflow run's records, under the session's
// sibling directory (subagentsDirForTranscript). workflowRunPrefix is the run
// dir's basename prefix ("wf_<hex>"), also the RunID itself.
const (
	workflowsDirName      = "workflows"
	workflowScriptsSubdir = "scripts"
	workflowRunPrefix     = "wf_"
	workflowJournalName   = "journal.jsonl"
	workflowScriptSuffix  = ".js"
)

// WorkflowAgentType is the agentType every workflow subagent's meta records —
// constant across all observed runs, so consumers may assume it rather than
// read each meta.
const WorkflowAgentType = "workflow-subagent"

// WorkflowRun locates one Workflow-tool run's on-disk records for a session.
type WorkflowRun struct {
	RunID   string // the run dir's basename, e.g. "wf_5e3cb808-2ac"
	Dir     string // <dir>/<session-id>/subagents/workflows/<RunID>
	Journal string // Dir/journal.jsonl
	Name    string // workflow name from the persisted script's filename; "" when no script matches
}

// AgentTranscript is the workflow subagent's own transcript file within this
// run's dir — the mtime source for activity/staleness. The id must be BARE
// (the <id> between "agent-" and ".jsonl"), exactly as the journal reports it.
func (w WorkflowRun) AgentTranscript(agentID string) string {
	if agentID == "" || strings.ContainsRune(agentID, filepath.Separator) || strings.ContainsRune(agentID, '/') {
		return ""
	}
	return filepath.Join(w.Dir, subagentFilePrefix+agentID+subagentJSONLSuffix)
}

// WorkflowRunsForTranscript derives the per-session workflow runs dir from the
// parent transcript path (<dir>/<session-id>.jsonl) and returns one WorkflowRun
// per wf_* subdirectory, in name order. Returns (nil, nil) when the dir is
// absent — the common case, a session that never ran a workflow. Each run's
// Name is resolved from the sibling scripts dir
// (<dir>/<session-id>/workflows/scripts/<name>-<RunID>.js); a run whose script
// is missing keeps Name "".
func WorkflowRunsForTranscript(transcriptPath string) ([]WorkflowRun, error) {
	if transcriptPath == "" {
		return nil, errors.New("transcript: empty path")
	}
	runsDir := filepath.Join(subagentsDirForTranscript(transcriptPath), workflowsDirName)
	dirents, err := os.ReadDir(runsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no workflow has ever run in this session
		}
		return nil, err
	}
	names := scriptNamesByRunID(transcriptPath)
	var runs []WorkflowRun
	for _, de := range dirents {
		if !de.IsDir() || !strings.HasPrefix(de.Name(), workflowRunPrefix) {
			continue
		}
		runID := de.Name()
		dir := filepath.Join(runsDir, runID)
		runs = append(runs, WorkflowRun{
			RunID:   runID,
			Dir:     dir,
			Journal: filepath.Join(dir, workflowJournalName),
			Name:    names[runID],
		})
	}
	return runs, nil
}

// scriptNamesByRunID lists the session's persisted workflow scripts
// (<dir>/<session-id>/workflows/scripts/) and maps each run id to the workflow
// name its filename carries: "<name>-<runid>.js" → name. Best-effort — a
// missing dir or an unmatched filename simply leaves names unresolved.
func scriptNamesByRunID(transcriptPath string) map[string]string {
	base := strings.TrimSuffix(transcriptPath, subagentJSONLSuffix)
	scriptsDir := filepath.Join(base, workflowsDirName, workflowScriptsSubdir)
	dirents, err := os.ReadDir(scriptsDir)
	if err != nil {
		return nil
	}
	names := map[string]string{}
	for _, de := range dirents {
		fn := de.Name()
		if de.IsDir() || !strings.HasSuffix(fn, workflowScriptSuffix) {
			continue
		}
		stem := strings.TrimSuffix(fn, workflowScriptSuffix)
		// The run id is the suffix after the last "-wf_" boundary: the script is
		// saved as <meta.name>-<runID>.js and runIDs begin with "wf_".
		i := strings.LastIndex(stem, "-"+workflowRunPrefix)
		if i <= 0 {
			continue
		}
		name, runID := stem[:i], stem[i+1:]
		if name == "" || runID == "" {
			continue
		}
		names[runID] = name
	}
	return names
}

// workflowJournalEntry is the subset of a journal line we parse. Result
// payloads (arbitrarily large) are deliberately not decoded.
type workflowJournalEntry struct {
	Type    string `json:"type"`
	AgentID string `json:"agentId"`
}

// Journal line types: an agent() call began, and its result landed. These are
// the ONLY two shapes observed across every recorded run; a workflow's own
// completion is not journaled (the consumer infers it from quiescence or the
// parent transcript's task notification).
const (
	workflowJournalStarted = "started"
	workflowJournalResult  = "result"
)

// WorkflowJournalSince reads the complete lines appended to a run's
// journal.jsonl since byteOffset and returns the agent ids newly started and
// newly resulted, plus the offset to resume from. Offset semantics are
// readNewLines's: only newline-terminated lines count (a line caught mid-write
// is re-read next call), and a truncated/replaced file restarts from 0.
// Unparseable lines are tolerated. Returns a non-nil error only on I/O failure
// — including a journal that does not exist yet (a just-launched run), which
// the caller should treat as "nothing started".
func WorkflowJournalSince(path string, byteOffset int64) (started, resulted []string, newOffset int64, err error) {
	complete, newOffset, err := readNewLines(path, byteOffset)
	if err != nil || len(complete) == 0 {
		return nil, nil, newOffset, err
	}
	for _, raw := range bytes.Split(complete, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var e workflowJournalEntry
		if json.Unmarshal(raw, &e) != nil || e.AgentID == "" {
			continue
		}
		switch e.Type {
		case workflowJournalStarted:
			started = append(started, e.AgentID)
		case workflowJournalResult:
			resulted = append(resulted, e.AgentID)
		}
	}
	return started, resulted, newOffset, nil
}
