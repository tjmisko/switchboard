package history

// Carried names: a session's name outlives the day-file it was set in.
//
// A `/name` is written once, as a single session_label event, and the daemon
// dedups every tick after it — so a session named yesterday evening and still
// running this morning has NO label event in today's file. Replaying today's
// window from today's events alone therefore loses the name: the lane reads as
// never-named, and a dashboard draws its whole bar as the "before first /name"
// lead-in even while the session is sitting there wearing the name.
//
// The repair is at read time rather than write time. A re-emit at rollover would
// only fix days that have not happened yet, would depend on the daemon being up
// at midnight, and would still lose the name on the first window a resumed
// session appears in. Looking back over the earlier day-files at read time fixes
// every window, including the ones already on disk.
//
// Carry-forward keys on SESSION ID only. A pid is reused within hours, so
// carrying a name across days on a pid would eventually paint a fresh session
// with a dead one's name; a session id never repeats, and a `claude --resume`
// keeps its id, which is exactly the case that should keep the name.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"time"
)

// carriedNameLookbackDays bounds how many day-files a carry-forward scan opens.
// The scan stops early once every unnamed lane is resolved, so the bound only
// binds when some session in the window was genuinely never named — the common
// case, since an unnamed session never resolves. A week is far longer than any
// session observed in practice, and each file is scanned with a substring
// prefilter (only session_label lines are parsed), so the floor cost is a
// sequential read, not a full decode of the day.
const carriedNameLookbackDays = 7

// sessionLabelMarker prefilters the lines a carry-forward scan bothers to
// decode. It matches the event type's value, not a `"type":"session_label"`
// field pair, so it holds regardless of the key order a writer emitted.
var sessionLabelMarker = []byte(EventSessionLabel)

// BackfillCarriedNames gives each lane the name its session was already carrying
// when the window opened. Lanes whose label history starts at their first instant
// are left alone; for the rest it scans the day-files older than `before` for the
// session's last recorded name and back-fills the unnamed lead-in with it, then
// re-derives Names and Name so every downstream consumer sees one consistent
// story.
//
// It mutates lanes in place. A zero `before` (an unbounded window, which already
// holds every label event there is) is a no-op, as is a window in which every
// lane is named from its first instant. An I/O error is returned but is not fatal
// to a caller: on error the lanes are simply left as they were built.
func BackfillCarriedNames(dir string, lanes []Swimlane, before time.Time) error {
	want := map[string]bool{}
	for _, lane := range lanes {
		if needsCarriedName(lane) {
			want[lane.SessionID] = true
		}
	}
	prior, err := priorLabels(dir, before, want)
	if err != nil {
		return err
	}
	applyCarriedNames(lanes, prior)
	return nil
}

// needsCarriedName reports whether a lane has an unnamed stretch at its start
// that an earlier day-file might explain: it is an identified session (a pid-only
// lead-in is not carried, see the package note) whose label history either does
// not exist or begins after the lane does.
func needsCarriedName(lane Swimlane) bool {
	if lane.SessionID == "" {
		return false
	}
	return len(lane.Labels) == 0 || lane.Labels[0].Start.After(lane.Start)
}

// applyCarriedNames back-fills each wanted lane's lead-in with the name it
// carried into the window. A carried name identical to the lane's first
// in-window label extends that span backwards instead of prepending a duplicate:
// the same name recorded twice (a daemon restart re-emits every live session's
// label) is one unbroken stretch of that name, not two.
func applyCarriedNames(lanes []Swimlane, prior map[string]string) {
	for i := range lanes {
		lane := &lanes[i]
		carried := prior[lane.SessionID]
		if carried == "" || !needsCarriedName(*lane) {
			continue
		}
		switch {
		case len(lane.Labels) == 0:
			lane.Labels = []LabelSpan{{Label: carried, Start: lane.Start, End: lane.End}}
		case lane.Labels[0].Label == carried:
			lane.Labels[0].Start = lane.Start
		default:
			lane.Labels = append([]LabelSpan{{
				Label: carried, Start: lane.Start, End: lane.Labels[0].Start,
			}}, lane.Labels...)
		}
		lane.Names = slugSpans(*lane)
		lane.Name = canonicalLaneName(*lane)
	}
}

// priorLabels finds, for each wanted session id, the last name recorded for it
// before `before`. Day-files are walked newest-first so the first hit for a
// session is its most recent name, and the walk stops as soon as every wanted
// session is resolved or carriedNameLookbackDays files have been opened.
func priorLabels(dir string, before time.Time, want map[string]bool) (map[string]string, error) {
	out := map[string]string{}
	if before.IsZero() || len(want) == 0 {
		return out, nil
	}
	days, err := Days(dir)
	if err != nil {
		return nil, err
	}
	opened := 0
	for i := len(days) - 1; i >= 0 && opened < carriedNameLookbackDays && len(out) < len(want); i-- {
		d, err := time.ParseInLocation("2006-01-02", days[i], time.Local)
		if err != nil {
			continue
		}
		// A file whose day begins after the window did cannot hold an earlier event.
		// The file for the window's own day IS opened: an event can land just before
		// the bound, and a legacy UTC-named file's contents are offset from its name.
		if d.After(before) {
			continue
		}
		opened++
		day, err := scanDayLabels(DayPath(dir, days[i]), before, want)
		if err != nil {
			return nil, err
		}
		for id, label := range day {
			if _, newer := out[id]; !newer {
				out[id] = label
			}
		}
	}
	return out, nil
}

// scanDayLabels returns the last name each wanted session was given in one
// day-file before `before`. Only lines mentioning the event type are decoded; a
// torn or foreign line is skipped exactly as ReadDay skips it, and a missing file
// is empty rather than an error.
func scanDayLabels(path string, before time.Time, want map[string]bool) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, sessionLabelMarker) {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type != EventSessionLabel || ev.SessionID == "" || ev.Label == "" {
			continue
		}
		if !ev.Ts.Before(before) || !want[ev.SessionID] {
			continue
		}
		out[ev.SessionID] = ev.Label // a later line in the same file is the newer name
	}
	return out, sc.Err()
}

// activityMarker prefilters the lines a carried-activity scan decodes, matching
// the event type's value exactly as sessionLabelMarker does for names.
var activityMarker = []byte(EventActivity)

// CarriedActivityState returns the operator's activity state ("idle" or
// "active") as of `at` — the last activity edge recorded strictly before it —
// or "" when no edge exists within the lookback (the state is then unknown).
//
// This is the activity stream's carry-forward, and it exists for the same
// reason names carry (see the package note): the daemon writes an edge only
// when the state CHANGES, so an operator who went idle at 23:49 and slept in
// has no edge in the next day-file until morning. Replaying that day from its
// own events alone leaves the overnight stretch stateless, and the old
// presumed-active seed turned it into fabricated presence. Callers express a
// found state by prepending a synthetic edge at their window start, which both
// ActivityTimeline and userActiveSpans then tile from.
//
// A carried "idle" is trusted at any age — idle is the absorbing state, a
// machine nobody touches stays idle. A carried "active" expires exactly like
// an in-window hold (activeHoldCap): an active edge hours before the window
// with no idle edge after it means the watcher died mid-claim, and carrying it
// would re-fabricate the phantom overnight presence this function exists to
// kill. An expired active reads as unknown ("").
func CarriedActivityState(dir string, at time.Time) (string, error) {
	if at.IsZero() {
		return "", nil
	}
	days, err := Days(dir)
	if err != nil {
		return "", err
	}
	opened := 0
	for i := len(days) - 1; i >= 0 && opened < carriedNameLookbackDays; i-- {
		d, err := time.ParseInLocation("2006-01-02", days[i], time.Local)
		if err != nil {
			continue
		}
		// A file whose day begins after `at` cannot hold an earlier edge. The file
		// for `at`'s own day IS opened: an edge can land just before the bound, and
		// a legacy UTC-named file's contents are offset from its name.
		if d.After(at) {
			continue
		}
		opened++
		state, ts, err := scanDayActivityState(DayPath(dir, days[i]), at)
		if err != nil {
			return "", err
		}
		if state == "" {
			continue
		}
		// newest-first walk: the first hit is the latest edge
		if state == activityActive && at.Sub(ts) > activeHoldCap {
			return "", nil // the claim expired before the window opened
		}
		return state, nil
	}
	return "", nil
}

// scanDayActivityState returns the last activity edge before `at` in one
// day-file (its state and timestamp), or "" when the file holds none.
// Malformed lines are skipped exactly as the other carry-forward scans skip
// them, and a missing file is empty rather than an error.
func scanDayActivityState(path string, at time.Time) (string, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", time.Time{}, nil
		}
		return "", time.Time{}, err
	}
	defer f.Close()

	state := ""
	var stateTs time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, activityMarker) {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type != EventActivity || (ev.To != activityIdle && ev.To != activityActive) {
			continue
		}
		if !ev.Ts.Before(at) {
			continue
		}
		state, stateTs = ev.To, ev.Ts // a later line in the same file is the newer edge
	}
	return state, stateTs, sc.Err()
}
