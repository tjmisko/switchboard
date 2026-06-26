package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// DayPath is the file an event for the given UTC day is written to.
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

// Days returns the UTC day keys present in dir, oldest-first.
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
		// Skip whole files outside the range (the file's day is its lower bound).
		d, _ := time.ParseInLocation("2006-01-02", day, time.UTC)
		if !to.IsZero() && !d.Before(to.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)) {
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
