package history

import (
	"encoding/json"
	"testing"
	"time"
)

// The carry-forward fixtures are anchored in LOCAL time: day-files are named for
// the local day (dayKey), and priorLabels compares a file's local midnight
// against the window bound, so a UTC-anchored fixture would drift into the wrong
// file under a non-UTC zone.
func localAt(day, hour int) time.Time {
	return time.Date(2026, 6, day, hour, 0, 0, 0, time.Local)
}

func dayOf(at time.Time) string { return at.Local().Format("2006-01-02") }

// labelLine renders a session_label event exactly as the sink writes one, so the
// scan is exercised against the real on-disk shape rather than a hand-rolled one.
func labelLine(t *testing.T, at time.Time, sessionID, label string) string {
	t.Helper()
	b, err := json.Marshal(Event{Ts: at, Type: EventSessionLabel, SessionID: sessionID, PID: 1, Agent: "claude", Label: label})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// working is one in-window transition for a session, enough to open its lane.
func working(at time.Time, sessionID string) Event {
	return Event{Ts: at, Type: EventTransition, PID: 1, SessionID: sessionID, Agent: "claude", Project: "sb", From: "idle", To: "working"}
}

// The overnight case this exists for: named yesterday evening, still running this
// morning, no label event anywhere in today's window. The lane must wear the name
// for its whole length rather than reading as never-named.
func TestBackfillCarriedNamesShouldNameAnOvernightLaneWhenTodayHasNoLabelEvent(t *testing.T) {
	dir := t.TempDir()
	named := localAt(25, 20)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "digest-status"))

	from := localAt(26, 0)
	laneStart, laneEnd := localAt(26, 8), localAt(26, 9)
	lanes := BuildSwimlanes([]Event{working(laneStart, "s1")}, laneEnd)
	if err := BackfillCarriedNames(dir, lanes, from); err != nil {
		t.Fatal(err)
	}

	l := lanes[0]
	if l.Name != "digest-status" {
		t.Errorf("lane name = %q, want the name it carried in from yesterday", l.Name)
	}
	if len(l.Names) != 1 {
		t.Fatalf("names = %+v, want one carried span", l.Names)
	}
	if !l.Names[0].Start.Equal(laneStart) || !l.Names[0].End.Equal(laneEnd) {
		t.Errorf("carried span = %+v, want the lane's full extent %v … %v", l.Names[0], laneStart, laneEnd)
	}
}

// A daemon restart re-emits every live session's label, so the same name can
// appear mid-window. That is one unbroken stretch of the name, not a rename: the
// existing span extends back to the lane's start rather than gaining a twin.
func TestBackfillCarriedNamesShouldExtendTheExistingSpanWhenTodayReEmitsTheSameName(t *testing.T) {
	dir := t.TempDir()
	named := localAt(25, 20)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "digest-status"))

	laneStart, reEmit, laneEnd := localAt(26, 8), localAt(26, 9), localAt(26, 10)
	evs := []Event{
		working(laneStart, "s1"),
		{Ts: reEmit, Type: EventSessionLabel, PID: 1, SessionID: "s1", Label: "digest-status"},
	}
	lanes := BuildSwimlanes(evs, laneEnd)
	if err := BackfillCarriedNames(dir, lanes, localAt(26, 0)); err != nil {
		t.Fatal(err)
	}

	names := lanes[0].Names
	if len(names) != 1 {
		t.Fatalf("names = %+v, want the one span, not a duplicate of the same name", names)
	}
	if !names[0].Start.Equal(laneStart) {
		t.Errorf("span start = %v, want the lane start %v", names[0].Start, laneStart)
	}
}

// Renamed inside the window: the carried name explains the lead-in and the new
// one takes over where it was set. Both files are on disk, as they would be in
// practice, so this also pins that today's own label is not mistaken for a prior
// one.
func TestBackfillCarriedNamesShouldPrecedeTheNewNameWhenTheSessionIsRenamedInWindow(t *testing.T) {
	dir := t.TempDir()
	named, renamed := localAt(25, 20), localAt(26, 9)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "digest-status"))
	writeDay(t, dir, dayOf(renamed), labelLine(t, renamed, "s1", "digest-rewrite"))

	laneStart, laneEnd := localAt(26, 8), localAt(26, 10)
	evs := []Event{
		working(laneStart, "s1"),
		{Ts: renamed, Type: EventSessionLabel, PID: 1, SessionID: "s1", Label: "digest-rewrite"},
	}
	lanes := BuildSwimlanes(evs, laneEnd)
	if err := BackfillCarriedNames(dir, lanes, localAt(26, 0)); err != nil {
		t.Fatal(err)
	}

	l := lanes[0]
	want := []LabelSpan{
		{Label: "digest-status", Start: laneStart, End: renamed},
		{Label: "digest-rewrite", Start: renamed, End: laneEnd},
	}
	if len(l.Names) != len(want) {
		t.Fatalf("names = %+v, want %d spans", l.Names, len(want))
	}
	for i, got := range l.Names {
		if got.Label != want[i].Label || !got.Start.Equal(want[i].Start) || !got.End.Equal(want[i].End) {
			t.Errorf("span %d = %+v, want %+v", i, got, want[i])
		}
	}
	if l.Name != "digest-rewrite" {
		t.Errorf("canonical name = %q, want the name set most recently", l.Name)
	}
}

// A lane already named from its first instant has no lead-in to explain, so an
// older name must not be prepended in front of it.
func TestBackfillCarriedNamesShouldLeaveALaneNamedFromItsFirstInstantAlone(t *testing.T) {
	dir := t.TempDir()
	named := localAt(25, 20)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "stale-name"))

	laneStart, laneEnd := localAt(26, 8), localAt(26, 10)
	evs := []Event{{Ts: laneStart, Type: EventSessionLabel, PID: 1, SessionID: "s1", Label: "fresh-name"}}
	lanes := BuildSwimlanes(evs, laneEnd)
	if err := BackfillCarriedNames(dir, lanes, localAt(26, 0)); err != nil {
		t.Fatal(err)
	}

	names := lanes[0].Names
	if len(names) != 1 || names[0].Label != "fresh-name" {
		t.Errorf("names = %+v, want only the name the lane already had", names)
	}
}

// Carry-forward keys on the session id alone. A pid is reused within hours, so a
// pid-only lead-in (a process that never fired a hook) must never inherit a name
// from whatever session held that pid yesterday.
func TestBackfillCarriedNamesShouldNotCarryOntoAPidOnlyLane(t *testing.T) {
	dir := t.TempDir()
	named := localAt(25, 20)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "yesterdays-session"))

	laneStart, laneEnd := localAt(26, 8), localAt(26, 10)
	evs := []Event{{Ts: laneStart, Type: EventSessionStart, PID: 1, Agent: "claude"}}
	lanes := BuildSwimlanes(evs, laneEnd)
	if err := BackfillCarriedNames(dir, lanes, localAt(26, 0)); err != nil {
		t.Fatal(err)
	}

	if l := lanes[0]; l.Name != "" || len(l.Labels) != 0 {
		t.Errorf("pid-only lane = %+v, want no carried name", l)
	}
}

// The lookback is bounded: a session that has not been renamed in longer than
// carriedNameLookbackDays files stops the scan rather than walking the whole
// retained log on every poll.
func TestBackfillCarriedNamesShouldStopAtTheLookbackBound(t *testing.T) {
	dir := t.TempDir()
	// Day 26 is the window; days 17..25 each hold a file, and the name lives in the
	// oldest — one file past the bound once the newer eight have been opened.
	named := localAt(17, 20)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "long-ago-name"))
	for day := 18; day <= 25; day++ {
		at := localAt(day, 12)
		writeDay(t, dir, dayOf(at), labelLine(t, at, "other", "unrelated"))
	}

	lanes := BuildSwimlanes([]Event{working(localAt(26, 8), "s1")}, localAt(26, 10))
	if err := BackfillCarriedNames(dir, lanes, localAt(26, 0)); err != nil {
		t.Fatal(err)
	}

	if got := lanes[0].Name; got != "" {
		t.Errorf("lane name = %q, want none: the name is older than the %d-file lookback", got, carriedNameLookbackDays)
	}
}

// The mirror of the bound test: the same shape, with the name inside the
// lookback, must still be found — so the test above pins the bound and not some
// unrelated reason the scan came up empty.
func TestBackfillCarriedNamesShouldCarryANameFromSeveralDaysBackWithinTheBound(t *testing.T) {
	dir := t.TempDir()
	named := localAt(21, 20)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "long-running-name"))
	for day := 22; day <= 25; day++ {
		at := localAt(day, 12)
		writeDay(t, dir, dayOf(at), labelLine(t, at, "other", "unrelated"))
	}

	lanes := BuildSwimlanes([]Event{working(localAt(26, 8), "s1")}, localAt(26, 10))
	if err := BackfillCarriedNames(dir, lanes, localAt(26, 0)); err != nil {
		t.Fatal(err)
	}

	if got := lanes[0].Name; got != "long-running-name" {
		t.Errorf("lane name = %q, want the name set 5 files back (inside the %d-file lookback)", got, carriedNameLookbackDays)
	}
}

// A carried label that is a prose title (the auto-generated one, not a `/name`
// slug) is kept in Labels and reaches Name, but must stay out of Names — which is
// the slug-only history a consumer renders as the /name spans.
func TestBackfillCarriedNamesShouldKeepACarriedProseTitleOutOfNames(t *testing.T) {
	dir := t.TempDir()
	named := localAt(25, 20)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "Debug agents not recording data"))

	lanes := BuildSwimlanes([]Event{working(localAt(26, 8), "s1")}, localAt(26, 10))
	if err := BackfillCarriedNames(dir, lanes, localAt(26, 0)); err != nil {
		t.Fatal(err)
	}

	l := lanes[0]
	if len(l.Names) != 0 {
		t.Errorf("names = %+v, want empty: a prose title is not a /name slug", l.Names)
	}
	if len(l.Labels) != 1 || l.Name != "Debug agents not recording data" {
		t.Errorf("labels = %+v name = %q, want the carried title as the lane's label", l.Labels, l.Name)
	}
}

// An unbounded window already contains every label event there is, so there is
// nothing before it to look back at.
func TestBackfillCarriedNamesShouldNoOpOnAnUnboundedWindow(t *testing.T) {
	dir := t.TempDir()
	named := localAt(25, 20)
	writeDay(t, dir, dayOf(named), labelLine(t, named, "s1", "digest-status"))

	lanes := BuildSwimlanes([]Event{working(localAt(26, 8), "s1")}, localAt(26, 10))
	if err := BackfillCarriedNames(dir, lanes, time.Time{}); err != nil {
		t.Fatal(err)
	}

	if got := lanes[0].Name; got != "" {
		t.Errorf("lane name = %q, want none: an unbounded window carries nothing", got)
	}
}
