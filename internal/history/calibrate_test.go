package history

import (
	"encoding/json"
	"testing"
	"time"
)

// The calibration replay, tested over a synthesized corpus rather than the real
// one. The command exists to be pointed at a month of somebody's activity log,
// which no test can have, so what is pinned here is the classification — which
// population a lane lands in, and what quantity is measured — not any particular
// number that fell out of one person's history.

// calAt builds a local timestamp on a day of the synthetic corpus (March 2026,
// kept clear of the July episode the suspect fixtures use).
func calAt(day, h, m int) time.Time {
	return time.Date(2026, 3, day, h, m, 0, 0, time.Local)
}

// calDay is the day-file key calAt(day, …) partitions into.
func calDay(day int) string { return calAt(day, 0, 0).Format("2006-01-02") }

// writeEvents lays down one day-file from typed events, so a fixture reads as the
// session shape it is testing rather than as a wall of JSON.
func writeEvents(t *testing.T, dir string, day int, evs ...Event) {
	t.Helper()
	lines := make([]string, 0, len(evs))
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	writeDay(t, dir, calDay(day), lines...)
}

// hasSession reports whether a population sampled the named session, for the
// fixtures where the shape under test necessarily produces other lanes too.
func hasSession(p Population, sessionID string) bool {
	for _, s := range p.Samples {
		if s.SessionID == sessionID {
			return true
		}
	}
	return false
}

func TestCalibrateSplitsUnclosedLanes(t *testing.T) {
	t.Run("should call a lane legitimate when the corpus carries its session past the bound", func(t *testing.T) {
		dir := t.TempDir()
		// A session that ran across midnight: cut at the bound on day 1, its
		// session_end sitting in day 2's file. Silent for 1h50m when the reader cut it.
		writeEvents(t, dir, 1,
			Event{Ts: calAt(1, 22, 0), Type: EventSessionStart, PID: 200, Agent: "claude"},
			Event{Ts: calAt(1, 22, 10), Type: EventTransition, PID: 200, SessionID: "crosser", To: "working"},
		)
		writeEvents(t, dir, 2,
			Event{Ts: calAt(2, 0, 30), Type: EventSessionEnd, PID: 200, SessionID: "crosser"},
		)

		cal, err := Calibrate(dir, calAt(3, 12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got := cal.LaneGhost.Count(); got != 0 {
			t.Errorf("ghost count = %d, want 0: %+v", got, cal.LaneGhost.Samples)
		}
		if got := cal.LaneLegit.Count(); got != 1 {
			t.Fatalf("legitimate count = %d, want 1: %+v", got, cal.LaneLegit.Samples)
		}
		if got := cal.LaneLegit.Max().Dur; got != 1*time.Hour+50*time.Minute {
			t.Errorf("silence = %v, want 1h50m", got)
		}
	})

	t.Run("should call a lane a ghost when nothing carries its session id past the bound", func(t *testing.T) {
		dir := t.TempDir()
		writeEvents(t, dir, 1,
			Event{Ts: calAt(1, 8, 0), Type: EventSessionStart, PID: 100, Agent: "claude", Project: "sb"},
			Event{Ts: calAt(1, 8, 5), Type: EventTransition, PID: 100, SessionID: "lost", To: "working"},
			// …and never heard from again, in this file or any other.
		)

		cal, err := Calibrate(dir, calAt(3, 12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got := cal.LaneLegit.Count(); got != 0 {
			t.Errorf("legitimate count = %d, want 0: %+v", got, cal.LaneLegit.Samples)
		}
		if got := cal.LaneGhost.Count(); got != 1 {
			t.Fatalf("ghost count = %d, want 1: %+v", got, cal.LaneGhost.Samples)
		}
		got := cal.LaneGhost.Min()
		if got.Dur != 15*time.Hour+55*time.Minute {
			t.Errorf("silence = %v, want 15h55m", got.Dur)
		}
		// The sample has to be findable again — an unattributable extreme is how the
		// previous calibration became unarguable.
		if got.Day != calDay(1) || got.SessionID != "lost" || got.PID != 100 || got.Name != "sb" {
			t.Errorf("sample = %+v, want it to name day 1 / lost / pid 100 / sb", got)
		}
	})

	t.Run("should call a lane legitimate when a pid-only session_end closes it past the bound", func(t *testing.T) {
		dir := t.TempDir()
		// The residual id-less shape the schema allows and the real corpus is full of:
		// a process the daemon discovered but no hook ever named, whose death is
		// recorded as a bare session_end carrying only the pid. An id-keyed rule alone
		// reads every one of these as a ghost.
		writeEvents(t, dir, 1,
			Event{Ts: calAt(1, 21, 0), Type: EventSessionStart, PID: 777, Agent: "claude"},
		)
		writeEvents(t, dir, 2,
			Event{Ts: calAt(2, 4, 0), Type: EventSessionEnd, PID: 777, Agent: "claude"},
		)

		cal, err := Calibrate(dir, calAt(3, 12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got := cal.LaneGhost.Count(); got != 0 {
			t.Errorf("ghost count = %d, want 0: %+v", got, cal.LaneGhost.Samples)
		}
		if got := cal.LaneLegit.Count(); got != 1 {
			t.Fatalf("legitimate count = %d, want 1: %+v", got, cal.LaneLegit.Samples)
		}
		if got := cal.LaneLegit.Max().Dur; got != 3*time.Hour {
			t.Errorf("silence = %v, want 3h", got)
		}
	})

	t.Run("should not let a different session on the same pid vouch for a lane", func(t *testing.T) {
		dir := t.TempDir()
		writeEvents(t, dir, 1,
			Event{Ts: calAt(1, 8, 0), Type: EventSessionStart, PID: 888, Agent: "claude"},
			Event{Ts: calAt(1, 8, 5), Type: EventTransition, PID: 888, SessionID: "died", To: "working"},
		)
		// The pid comes back the next day carrying a DIFFERENT session id: the pid was
		// reused, which proves nothing about the session that held it yesterday.
		writeEvents(t, dir, 2,
			Event{Ts: calAt(2, 9, 0), Type: EventTransition, PID: 888, SessionID: "someone-else", To: "working"},
		)

		cal, err := Calibrate(dir, calAt(3, 12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got := cal.LaneLegit.Count(); got != 0 {
			t.Fatalf("legitimate count = %d, want 0 — a reused pid vouches for nothing: %+v", got, cal.LaneLegit.Samples)
		}
		if !hasSession(cal.LaneGhost, "died") {
			t.Errorf("ghosts = %+v, want the day-1 lane whose pid was reused among them", cal.LaneGhost.Samples)
		}
	})

	t.Run("should not sample a lane a session_end closed inside its own day", func(t *testing.T) {
		dir := t.TempDir()
		writeEvents(t, dir, 1,
			Event{Ts: calAt(1, 6, 0), Type: EventSessionStart, PID: 606, Agent: "claude"},
			Event{Ts: calAt(1, 6, 0), Type: EventTransition, PID: 606, SessionID: "closed", To: "working"},
			Event{Ts: calAt(1, 20, 0), Type: EventSessionEnd, PID: 606, SessionID: "closed"},
		)

		cal, err := Calibrate(dir, calAt(3, 12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if cal.Lanes != 1 {
			t.Errorf("Lanes = %d, want the closed lane to still be counted as replayed", cal.Lanes)
		}
		if n := cal.LaneLegit.Count() + cal.LaneGhost.Count(); n != 0 {
			t.Errorf("sampled %d lane(s), want 0 — a lane the reader never had to close cannot be flagged", n)
		}
	})
}

// The measure the caps are compared against changed when the last-evidence fix
// landed: it is the silence since the lane last emitted anything, not the length
// of its final status interval. A re-derivation that measures the old quantity
// re-derives the old number.
func TestCalibrateMeasuresSilenceNotIntervalLength(t *testing.T) {
	t.Run("should measure from the last evidence when usage accrues inside an unclosed interval", func(t *testing.T) {
		dir := t.TempDir()
		writeEvents(t, dir, 1,
			Event{Ts: calAt(1, 6, 0), Type: EventSessionStart, PID: 300, Agent: "claude"},
			Event{Ts: calAt(1, 6, 0), Type: EventTransition, PID: 300, SessionID: "loud", To: "working"},
			// One "working" interval held from 06:00 to the bound — 18h long — but the
			// session was still reporting usage at 21:00, so it was silent for 3h.
			Event{Ts: calAt(1, 21, 0), Type: EventUsageSample, PID: 300, SessionID: "loud",
				Model: "claude-sonnet-4-5", TokIn: 1000, TokOut: 100},
		)

		cal, err := Calibrate(dir, calAt(3, 12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got := cal.LaneGhost.Count(); got != 1 {
			t.Fatalf("ghost count = %d, want 1: %+v", got, cal.LaneGhost.Samples)
		}
		if got := cal.LaneGhost.Max().Dur; got != 3*time.Hour {
			t.Errorf("silence = %v, want 3h (18h is the interval length, the quantity this replaced)", got)
		}
	})
}

func TestCalibrateSplitsSubagentSpans(t *testing.T) {
	t.Run("should split spans by whether a subagent_stop closed them", func(t *testing.T) {
		dir := t.TempDir()
		writeEvents(t, dir, 1,
			Event{Ts: calAt(1, 9, 0), Type: EventSessionStart, PID: 400, Agent: "claude"},
			Event{Ts: calAt(1, 9, 0), Type: EventTransition, PID: 400, SessionID: "fanout", To: "delegating"},
			Event{Ts: calAt(1, 10, 0), Type: EventSubagentSpawn, PID: 400, SessionID: "fanout", AgentID: "a1", AgentType: "Explore"},
			Event{Ts: calAt(1, 10, 30), Type: EventSubagentStop, PID: 400, SessionID: "fanout", AgentID: "a1", AgentType: "Explore"},
			// a2 never lands its stop, so finish() caps it at the lane's bound.
			Event{Ts: calAt(1, 11, 0), Type: EventSubagentSpawn, PID: 400, SessionID: "fanout", AgentID: "a2", AgentType: "general-purpose"},
		)

		cal, err := Calibrate(dir, calAt(3, 12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got := cal.SpanPaired.Count(); got != 1 {
			t.Fatalf("paired count = %d, want 1: %+v", got, cal.SpanPaired.Samples)
		}
		if got := cal.SpanPaired.Max(); got.Dur != 30*time.Minute || got.Name != "Explore" {
			t.Errorf("paired sample = %+v, want 30m on Explore", got)
		}
		if got := cal.SpanCapped.Count(); got != 1 {
			t.Fatalf("reader-capped count = %d, want 1: %+v", got, cal.SpanCapped.Samples)
		}
		if got := cal.SpanCapped.Min().Dur; got != 13*time.Hour {
			t.Errorf("reader-capped span = %v, want 13h (11:00 to the bound)", got)
		}
	})
}

func TestCalibrateSkipsTheDayInProgress(t *testing.T) {
	t.Run("should replay only days whose next midnight has passed", func(t *testing.T) {
		dir := t.TempDir()
		writeEvents(t, dir, 1,
			Event{Ts: calAt(1, 8, 0), Type: EventSessionStart, PID: 1, Agent: "claude"},
			Event{Ts: calAt(1, 8, 0), Type: EventTransition, PID: 1, SessionID: "yesterday", To: "working"},
			Event{Ts: calAt(1, 23, 50), Type: EventSessionEnd, PID: 1, SessionID: "yesterday"},
		)
		// A session live right now, five minutes into its current status. Replaying
		// today to a midnight that has not happened yet would score it a ghost with
		// hours of silence it never had.
		writeEvents(t, dir, 2,
			Event{Ts: calAt(2, 11, 0), Type: EventSessionStart, PID: 2, Agent: "claude"},
			Event{Ts: calAt(2, 11, 55), Type: EventTransition, PID: 2, SessionID: "live", To: "working"},
		)

		cal, err := Calibrate(dir, calAt(2, 12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(cal.Days) != 1 || cal.Days[0] != calDay(1) {
			t.Fatalf("Days = %v, want only the completed day %s", cal.Days, calDay(1))
		}
		if n := cal.LaneGhost.Count(); n != 0 {
			t.Errorf("ghost count = %d, want 0 — the live session must not be sampled: %+v", n, cal.LaneGhost.Samples)
		}
	})
}

func TestCalibrateEmptyCorpus(t *testing.T) {
	t.Run("should report nothing when the log directory holds no day-files", func(t *testing.T) {
		cal, err := Calibrate(t.TempDir(), calAt(3, 12, 0))
		if err != nil {
			t.Fatalf("an empty log is the normal disabled case, not an error: %v", err)
		}
		if len(cal.Days) != 0 || cal.Lanes != 0 {
			t.Errorf("Calibration = %+v, want an empty replay", cal)
		}
	})
}

// The verdict is what makes the command actionable: the band is the argument for a
// cap, but the error counts are the thing that is either still zero or is not.
func TestVerdictScoresAThreshold(t *testing.T) {
	sample := func(d time.Duration) CalibrationSample { return CalibrationSample{Dur: d} }
	legit := Population{Samples: []CalibrationSample{sample(time.Minute), sample(time.Hour), sample(2*time.Hour + 25*time.Minute)}}
	ghost := Population{Samples: []CalibrationSample{sample(9 * time.Hour), sample(12 * time.Hour)}}
	cal := Calibration{LaneLegit: legit, LaneGhost: ghost}

	t.Run("should report no errors when the threshold sits inside the band", func(t *testing.T) {
		v := cal.LaneVerdict(4 * time.Hour)
		if v.FalsePositives != 0 || v.FalseNegatives != 0 {
			t.Errorf("verdict = %+v, want a clean split", v)
		}
		if !v.Band.Separated() || v.Band.Lo != 2*time.Hour+25*time.Minute || v.Band.Hi != 9*time.Hour {
			t.Errorf("band = %+v, want 2h25m … 9h", v.Band)
		}
		if got := v.Band.Position(4 * time.Hour); got < 0 || got > 1 {
			t.Errorf("position = %v, want the threshold inside the band", got)
		}
	})

	t.Run("should count a legitimate sample as a false positive when the threshold is too low", func(t *testing.T) {
		v := cal.LaneVerdict(time.Hour)
		if v.FalsePositives != 2 {
			t.Errorf("false positives = %d, want 2 (the cap is inclusive, so 1h counts)", v.FalsePositives)
		}
		if got := v.Band.Position(time.Hour); got >= 0 {
			t.Errorf("position = %v, want a negative reading rather than a clamp to the band's edge", got)
		}
	})

	t.Run("should count a ghost as a false negative when the threshold is too high", func(t *testing.T) {
		v := cal.LaneVerdict(10 * time.Hour)
		if v.FalseNegatives != 1 {
			t.Errorf("false negatives = %d, want 1", v.FalseNegatives)
		}
	})

	t.Run("should report no band when the two populations overlap", func(t *testing.T) {
		overlapping := Calibration{LaneLegit: legit, LaneGhost: Population{Samples: []CalibrationSample{sample(time.Hour)}}}
		v := overlapping.LaneVerdict(4 * time.Hour)
		if v.Band.Separated() {
			t.Errorf("band = %+v, want no separation when a ghost sits below the legitimate ceiling", v.Band)
		}
		if v.Band.Width() != 0 {
			t.Errorf("width = %v, want 0 for an inverted band", v.Band.Width())
		}
	})
}

func TestPopulationOrderStatistics(t *testing.T) {
	sample := func(d time.Duration) CalibrationSample { return CalibrationSample{Dur: d} }

	t.Run("should take the middle sample when the count is odd", func(t *testing.T) {
		p := Population{Samples: []CalibrationSample{sample(time.Minute), sample(5 * time.Minute), sample(time.Hour)}}
		if got := p.Median(); got != 5*time.Minute {
			t.Errorf("median = %v, want 5m", got)
		}
	})

	t.Run("should average the two middle samples when the count is even", func(t *testing.T) {
		p := Population{Samples: []CalibrationSample{sample(time.Minute), sample(3 * time.Minute)}}
		if got := p.Median(); got != 2*time.Minute {
			t.Errorf("median = %v, want 2m", got)
		}
	})

	t.Run("should report zeroes for an empty population rather than panicking", func(t *testing.T) {
		var p Population
		if p.Count() != 0 || p.Median() != 0 || p.Min().Dur != 0 || p.Max().Dur != 0 {
			t.Errorf("empty population = %+v, want zeroes throughout", p)
		}
	})
}
