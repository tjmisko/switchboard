package history

import (
	"bufio"
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
	events, err := ReadRange(dir, time.Time{}, time.Time{})
	if err != nil {
		return nil, nil, err
	}
	for _, ev := range events {
		if ev.SessionID != sessionID {
			continue
		}
		key := eventAgentKey(ev)
		if key == "" {
			continue
		}
		switch ev.Type {
		case EventSubagentSpawn:
			spawned[key] = true
		case EventSubagentStop:
			stopped[key] = true
		}
	}
	return spawned, stopped, nil
}
