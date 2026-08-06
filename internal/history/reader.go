package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// DayPath is the file an event for the given local day is written to.
func DayPath(dir, day string) string {
	return filepath.Join(dir, day+".jsonl")
}

// ReadDay reads and decodes every event in one day-file, tolerating a torn final
// line (a crash mid-append) by skipping any line that does not parse. A missing
// file is not an error — it returns an empty slice, since "no events that day"
// is the normal case.
func ReadDay(dir, day string) ([]Event, error) {
	f, err := os.Open(DayPath(dir, day))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var evs []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue // tolerate a torn line (crash mid-append)
		}
		if ev.Type == "" {
			continue // a foreign line that happens to be valid JSON: `type` is guaranteed-present for a real event
		}
		evs = append(evs, ev)
	}
	return evs, sc.Err()
}

// Days returns the local day keys present in dir, oldest-first.
func Days(dir string) ([]string, error) {
	files, err := listDayFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	days := make([]string, len(files))
	for i, f := range files {
		days[i] = dayKey(f.date)
	}
	return days, nil
}

// ReadRange reads every event whose timestamp falls in [from, to), across the
// day-files the range spans, in chronological (file then line) order. A zero
// `from` means "from the earliest file"; a zero `to` means "through the latest".
func ReadRange(dir string, from, to time.Time) ([]Event, error) {
	days, err := Days(dir)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, day := range days {
		// Skip whole files that cannot overlap [from, to). The file is named for a
		// local day, but pad a day on each side before skipping: an event can land
		// near a boundary, and the dir may still hold legacy UTC-named files whose
		// contents are offset from their name by the zone offset. Events are filtered
		// to the exact window below regardless, so the pad only governs which files
		// are opened, never which events are returned.
		d, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			continue
		}
		if !to.IsZero() && !d.Before(to.AddDate(0, 0, 1)) {
			continue
		}
		if !from.IsZero() && d.AddDate(0, 0, 2).Before(from) {
			continue
		}
		evs, err := ReadDay(dir, day)
		if err != nil {
			return nil, err
		}
		for _, ev := range evs {
			if !from.IsZero() && ev.Ts.Before(from) {
				continue
			}
			if !to.IsZero() && !ev.Ts.Before(to) {
				continue
			}
			out = append(out, ev)
		}
	}
	return out, nil
}

// PriorSubagentState scans the history log and returns, for sessionID, the set
// of subagent agent_ids already recorded as spawned and as stopped. Keyed by
// AgentID when present, else ToolUseID (eventAgentKey). Used to prime the fanout
// Observer's seen-set so previously-emitted spawns are never re-emitted after a
// daemon restart or a `claude --resume`. Events for other sessions are ignored,
// and a spawn/stop carrying neither key contributes nothing. A zero sessionID
// (or an empty/absent log) yields empty sets, not an error.
func PriorSubagentState(dir, sessionID string) (spawned, stopped map[string]bool, err error) {
	spawned = map[string]bool{}
	stopped = map[string]bool{}
	if sessionID == "" {
		return spawned, stopped, nil
	}
	days, err := Days(dir)
	if err != nil {
		return nil, nil, err
	}
	// The needle is the JSON-ENCODED session id, quotes included, not the bare
	// string: quoting is what stops session "s1" from matching `"session_id":"s10"`,
	// and it keeps the needle correct for an id that would need escaping.
	needle, err := json.Marshal(sessionID)
	if err != nil {
		return nil, nil, err
	}
	for _, day := range days {
		if err := scanCandidateLines(DayPath(dir, day), needle, subagentNeedle, func(ev Event) {
			if ev.SessionID != sessionID {
				return
			}
			key := eventAgentKey(ev)
			if key == "" {
				return
			}
			switch ev.Type {
			case EventSubagentSpawn:
				spawned[key] = true
			case EventSubagentStop:
				stopped[key] = true
			}
		}); err != nil {
			return nil, nil, err
		}
	}
	return spawned, stopped, nil
}

// subagentNeedle is the substring every subagent_spawn/subagent_stop line carries
// in its `type`, and no other event type does.
var subagentNeedle = []byte("subagent_")

// workflowNeedle is the same guard for workflow_start/workflow_stop. Unlike
// subagentNeedle it is NOT exclusive to the two types that matter: `workflow_run_id`
// is a field name, so every event a workflow's agents emit matches it too. That
// costs decodes, never correctness — the pre-filter only needs a necessary
// condition, and the switch on ev.Type rejects the rest.
var workflowNeedle = []byte("workflow_")

// scanCandidateLines streams one day-file and decodes ONLY the lines that could
// possibly be an event of the type `typeNeedle` names, for the session `idNeedle`
// names, handing each to fn.
//
// The two byte-scans before the unmarshal are the whole point. The caller's
// question is about one session, but a day-file is dominated by other sessions'
// transitions and usage samples, so decoding every line to discard nearly all of
// them made the cost of the answer scale with total retained history rather than
// with the session. Both needles are necessary conditions for a line to matter —
// a matching event must carry the quoted id and must carry the type substring —
// so skipping a line that lacks either drops nothing. The converse is not
// assumed: a line that contains both is still decoded and still checked against
// SessionID and Type, because the needles can appear in other fields (a cwd, a
// subject) and a substring match is not a parse.
//
// A torn final line is tolerated exactly as ReadDay tolerates it, and a file that
// vanished between the Days listing and the open is not an error.
func scanCandidateLines(path string, idNeedle, typeNeedle []byte, fn func(Event)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, idNeedle) || !bytes.Contains(line, typeNeedle) {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue // tolerate a torn line (crash mid-append)
		}
		fn(ev)
	}
	return sc.Err()
}

// PriorWorkflowState is PriorSubagentState's twin for workflow runs: the set of
// WorkflowRunIDs already recorded as started and as stopped for sessionID. It
// primes the Observer's per-run seen-set so a daemon restart mid-workflow does
// not re-emit workflow_start for a run whose records it is seeing again for the
// first time (the run dirs, like subagent metas, are never deleted).
//
// It is a twin in cost as well as in contract: it runs the same byte pre-filter,
// for the same reason. It used to answer this one-session question by decoding
// the entire archive, and because seedFor calls BOTH halves, that single
// unfiltered read set the price of the whole seed no matter how cheap the
// subagent half became. The seed is paid per newly-seen session and one of its
// two call sites — the lazy backstop in Observer.reconcile, which a hook trigger
// or a session discovered mid-tick reaches — runs with the store lock held.
func PriorWorkflowState(dir, sessionID string) (started, stopped map[string]bool, err error) {
	started = map[string]bool{}
	stopped = map[string]bool{}
	if sessionID == "" {
		return started, stopped, nil
	}
	days, err := Days(dir)
	if err != nil {
		return nil, nil, err
	}
	needle, err := json.Marshal(sessionID) // the JSON-ENCODED id; see PriorSubagentState
	if err != nil {
		return nil, nil, err
	}
	for _, day := range days {
		if err := scanCandidateLines(DayPath(dir, day), needle, workflowNeedle, func(ev Event) {
			if ev.SessionID != sessionID || ev.WorkflowRunID == "" {
				return
			}
			switch ev.Type {
			case EventWorkflowStart:
				started[ev.WorkflowRunID] = true
			case EventWorkflowStop:
				stopped[ev.WorkflowRunID] = true
			}
		}); err != nil {
			return nil, nil, err
		}
	}
	return started, stopped, nil
}
