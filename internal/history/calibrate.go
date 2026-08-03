package history

import (
	"sort"
	"time"
)

// Re-deriving the two suspect caps from a real corpus.
//
// DefaultSuspectTrailingCap and DefaultSuspectSubagentCap were frozen against
// figures that existed nowhere but the prose in suspect.go — 651 lanes, a largest
// legitimate silence of 2h25m25s, a smallest ghost of 8h57m58s, 204 paired
// subagent spans against 25 reader-capped ones — with no script anywhere that
// produced them. Anyone wanting to argue with either constant had to rebuild the
// whole analysis first, which in practice means nobody argues and the numbers
// calcify into folklore. This file is that script.
//
// It is advisory and deliberately unwired. The corpus it needs is a month of
// somebody's real activity log, which the repo does not and must not carry, so no
// test may depend on it: TestSuspectCapDefaults still pins the constants outright,
// and moving one stays a deliberate, reviewed edit. What this adds is the ability
// to check, in one command, whether the corpus still says what the comment claims.

// CalibrationSample is one measured duration tagged with enough identity to find
// the lane again. The identity is what makes an outlier arguable: "the largest
// legitimate silence is 2h25m" is only a checkable claim if you can open the
// day-file and look at the session that produced it.
type CalibrationSample struct {
	Day       string // the day-file it was replayed from
	SessionID string // empty for a lane no hook ever named
	PID       int
	Name      string // the lane's display name, else its project
	Dur       time.Duration
}

// Population is one measured set, ascending by Dur. Only the extremes and the
// middle are reported, never a mean: the whole argument is about where two
// populations sit relative to each other, and a single 20-hour ghost would drag a
// mean clean across the band while leaving every real number exactly where it was.
type Population struct {
	Samples []CalibrationSample
}

// Count is how many samples fell in this population.
func (p Population) Count() int { return len(p.Samples) }

// Min and Max are the population's extremes — one of which is always a band
// endpoint. Both return the zero sample (Dur 0) for an empty population, so a
// caller that forgot to check Count reads "0s" rather than panicking.
func (p Population) Min() CalibrationSample {
	if len(p.Samples) == 0 {
		return CalibrationSample{}
	}
	return p.Samples[0]
}

func (p Population) Max() CalibrationSample {
	if len(p.Samples) == 0 {
		return CalibrationSample{}
	}
	return p.Samples[len(p.Samples)-1]
}

// Median is the middle of the population (the mean of the two middle samples when
// the count is even). It says how far the extreme that borders the band sits from
// the bulk — a legitimate maximum of 2h25m means something different when the
// median is 4m than when the median is 2h.
func (p Population) Median() time.Duration {
	n := len(p.Samples)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return p.Samples[n/2].Dur
	}
	return (p.Samples[n/2-1].Dur + p.Samples[n/2].Dur) / 2
}

// AtOrAbove counts the samples a cap of d would flag. The check's comparison is
// inclusive (`gap >= cap`), so this counts the same way it does — off by one
// sample exactly at the threshold is the difference between a defensible cap and
// one that flags a real session.
func (p Population) AtOrAbove(d time.Duration) int {
	n := 0
	for _, s := range p.Samples {
		if s.Dur >= d {
			n++
		}
	}
	return n
}

// Below counts the samples a cap of d would let through.
func (p Population) Below(d time.Duration) int { return len(p.Samples) - p.AtOrAbove(d) }

// Band is the empty stretch between the top of the legitimate population and the
// bottom of the pathological one: the entire region a cap may sit in without
// either flagging something real or missing something broken. A cap is a claim
// about this band and about nothing else — it is what makes 4h defensible and 30m
// or 12h not (see DefaultSuspectTrailingCap).
type Band struct {
	Lo time.Duration // the legitimate population's maximum
	Hi time.Duration // the pathological population's minimum
}

// Separated reports whether the corpus still puts the two populations on opposite
// sides of a gap. When it does not, no cap is defensible at all — which is a
// finding about the corpus (or about the measure), not an error to swallow.
func (b Band) Separated() bool { return b.Hi > b.Lo }

// Width is the gap, or zero when the populations overlap.
func (b Band) Width() time.Duration {
	if !b.Separated() {
		return 0
	}
	return b.Hi - b.Lo
}

// Headroom is how many times the legitimate ceiling a threshold sits at — the
// "~1.65x" in the DefaultSuspectTrailingCap comment. Zero when the legitimate
// population is empty and there is no ceiling to have headroom over.
func (b Band) Headroom(threshold time.Duration) float64 {
	if b.Lo <= 0 {
		return 0
	}
	return float64(threshold) / float64(b.Lo)
}

// Position is where a threshold falls across the band: 0 at Lo, 1 at Hi. It is
// deliberately not clamped — a value outside [0, 1] is the whole finding, and
// clamping it would report a miscalibrated cap as one sitting exactly on an edge.
func (b Band) Position(threshold time.Duration) float64 {
	if !b.Separated() {
		return 0
	}
	return float64(threshold-b.Lo) / float64(b.Hi-b.Lo)
}

// Verdict is what a threshold would actually do to the corpus: the band it has to
// sit in, plus both error counts. Neither error is the cheap one. Summarize holds
// every total to the trusted end, so a false positive silently undercounts real
// work exactly as a false negative silently inflates it, and a cap is defensible
// only while both counts are zero.
type Verdict struct {
	Threshold      time.Duration
	Band           Band
	FalsePositives int // legitimate samples the threshold would flag
	FalseNegatives int // pathological samples it would let through
}

// Calibration is one replay of a corpus: the two lane populations and the two
// subagent-span populations that the caps sit between.
type Calibration struct {
	Dir   string
	Days  []string // the complete days replayed, oldest first
	Lanes int      // every lane the replay produced, closed or not

	// LaneLegit and LaneGhost split the lanes the reader had to close at the bound
	// by whether the corpus vouches for them (see outlivedBound). The measured
	// quantity is SILENCE — lane.End minus laneEvidence, the exact gap
	// FlagSuspectLanes compares against the cap.
	LaneLegit Population
	LaneGhost Population

	// SpanPaired and SpanCapped split every subagent span by how it ended: a
	// subagent_stop paired with its spawn, or finish() capping it at the lane's
	// bound because none ever arrived.
	SpanPaired Population
	SpanCapped Population
}

// LaneVerdict scores a candidate lane cap against the replayed corpus.
func (c Calibration) LaneVerdict(threshold time.Duration) Verdict {
	return verdict(c.LaneLegit, c.LaneGhost, threshold)
}

// SpanVerdict scores a candidate subagent cap the same way.
func (c Calibration) SpanVerdict(threshold time.Duration) Verdict {
	return verdict(c.SpanPaired, c.SpanCapped, threshold)
}

func verdict(legit, pathological Population, threshold time.Duration) Verdict {
	return Verdict{
		Threshold:      threshold,
		Band:           Band{Lo: legit.Max().Dur, Hi: pathological.Min().Dur},
		FalsePositives: legit.AtOrAbove(threshold),
		FalseNegatives: pathological.Below(threshold),
	}
}

// Calibrate replays every complete day-file in dir through BuildSwimlanes at
// `end` = that day's next local midnight — the same shape a closed-day
// `switchboard-ctl timeline --day` query has — and sorts what comes out into the
// four populations the two caps sit between.
//
// `now` is injected rather than read so a test can pin which days count as
// complete; pass time.Now().
func Calibrate(dir string, now time.Time) (Calibration, error) {
	days, err := Days(dir)
	if err != nil {
		return Calibration{}, err
	}
	cal := Calibration{Dir: dir}
	// The whole corpus is read before any of it is replayed, because classifying a
	// lane needs to know what happened AFTER the day it lives in — a session cut at
	// midnight is vouched for by the next morning's file, which the day's own replay
	// cannot see.
	byDay := make(map[string][]Event, len(days))
	seen := newCorpusEvidence()
	for _, day := range days {
		evs, err := ReadDay(dir, day)
		if err != nil {
			return Calibration{}, err
		}
		byDay[day] = evs
		seen.record(evs)
	}

	for _, day := range days {
		start, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			continue // Days already filtered these out; defensive
		}
		end := start.AddDate(0, 0, 1)
		// A day still in progress is skipped whole. Replaying it to a midnight that
		// has not happened yet would synthesize hours of "silence" out of a future the
		// reader cannot see, dropping every session that is running right now into the
		// ghost population — the same bad inference from missing data the suspect check
		// exists to catch, which the calibration must not commit while measuring it.
		if end.After(now) {
			continue
		}
		cal.Days = append(cal.Days, day)
		lanes := BuildSwimlanes(byDay[day], end)
		cal.Lanes += len(lanes)
		for _, lane := range lanes {
			cal.collectLane(day, lane, seen)
			cal.collectSpans(day, lane)
		}
	}

	cal.LaneLegit.sortAscending()
	cal.LaneGhost.sortAscending()
	cal.SpanPaired.sortAscending()
	cal.SpanCapped.sortAscending()
	return cal, nil
}

// collectLane measures one lane's silence, if it is a lane the reader had to close
// — the others can never be flagged however long they are, so they are not part of
// the question.
//
// The measured quantity is the gap since laneEvidence, NOT the trailing interval's
// length. The two coincided on the corpus the caps were frozen against, and
// suspect.go says so, but they stopped being interchangeable the moment
// usage_sample began proving a session alive inside a status interval it never
// closed. Re-deriving against interval length would be re-deriving the old number.
func (c *Calibration) collectLane(day string, lane Swimlane, seen corpusEvidence) {
	if !lane.closedByBound || len(lane.Intervals) == 0 {
		return
	}
	last := lane.Intervals[len(lane.Intervals)-1]
	s := CalibrationSample{
		Day:       day,
		SessionID: lane.SessionID,
		PID:       lane.PID,
		Name:      calibrationLaneName(lane),
		Dur:       lane.End.Sub(laneEvidence(lane, last)),
	}
	if seen.outlivedBound(lane) {
		c.LaneLegit.Samples = append(c.LaneLegit.Samples, s)
		return
	}
	c.LaneGhost.Samples = append(c.LaneGhost.Samples, s)
}

// collectSpans measures every subagent span on a lane, however the lane itself was
// closed: a span its subagent_stop closed is real work whether or not its parent
// went on to ghost.
func (c *Calibration) collectSpans(day string, lane Swimlane) {
	for _, sp := range lane.Subagents {
		s := CalibrationSample{
			Day:       day,
			SessionID: lane.SessionID,
			PID:       lane.PID,
			Name:      calibrationSpanName(sp),
			Dur:       sp.End.Sub(sp.Start),
		}
		if sp.closedByBound {
			c.SpanCapped.Samples = append(c.SpanCapped.Samples, s)
			continue
		}
		c.SpanPaired.Samples = append(c.SpanPaired.Samples, s)
	}
}

// corpusEvidence is the last instant anything was heard from a session, indexed
// the same two ways BuildSwimlanes routes an event: by session id, and — for the
// id-less lifecycle events the schema explicitly allows (a session_start fired at
// process discovery, a session_end for a process that never ran a hook) — by pid.
type corpusEvidence struct {
	bySession map[string]time.Time
	byAnonPID map[int]time.Time // only events carrying NO session id
}

func newCorpusEvidence() corpusEvidence {
	return corpusEvidence{bySession: map[string]time.Time{}, byAnonPID: map[int]time.Time{}}
}

func (e corpusEvidence) record(evs []Event) {
	for _, ev := range evs {
		if ev.SessionID != "" {
			if ev.Ts.After(e.bySession[ev.SessionID]) {
				e.bySession[ev.SessionID] = ev.Ts
			}
			continue
		}
		if ev.PID != 0 && ev.Ts.After(e.byAnonPID[ev.PID]) {
			e.byAnonPID[ev.PID] = ev.Ts
		}
	}
}

// outlivedBound reports whether the corpus proves the session was still there
// after the bound its lane was cut at.
//
// The frozen calibration phrased this as "a real session_end closed it elsewhere
// in the corpus", and a session_end is the strongest form of the proof — but it is
// not the only form, and taking it as the only one misreads the most common
// legitimate shape there is. A session running right now that started before
// midnight has no session_end anywhere yet, though yesterday's replay cut it at
// midnight while it was demonstrably alive; scoring that a ghost drops a
// two-minute silence into the ghost population and collapses the band to nothing.
// Any later event carrying the id proves what a session_end proves, so any later
// event counts.
//
// The pid arm covers the id-less half of the same proof, and it is not optional:
// the real corpus closes lanes with bare `{"type":"session_end","pid":N}` lines
// for processes that died before any hook named them, and an id-only rule reads
// every one of those as a ghost. It is narrowed to events carrying NO session id,
// which is exactly the set BuildSwimlanes itself would have routed into this lane
// had the day-file not ended — an event naming a DIFFERENT session on the same pid
// means the pid was reused, and proves nothing about this lane.
//
// The one shape this reads wrong is a lane whose death really was lost and whose
// session id (or pid) is claimed again later: the later run vouches for the ghost.
// That surfaces as a legitimate outlier sitting high in the band — visible,
// arguable, and in the failure direction an advisory tool should prefer.
func (e corpusEvidence) outlivedBound(lane Swimlane) bool {
	if t, ok := e.bySession[lane.SessionID]; ok && lane.SessionID != "" && !t.Before(lane.End) {
		return true
	}
	t, ok := e.byAnonPID[lane.PID]
	return ok && lane.PID != 0 && !t.Before(lane.End)
}

func (p *Population) sortAscending() {
	sort.SliceStable(p.Samples, func(i, j int) bool { return p.Samples[i].Dur < p.Samples[j].Dur })
}

// calibrationLaneName is how a lane is identified in the report: its display name,
// else its project, else nothing (the pid on the same line still finds it).
func calibrationLaneName(lane Swimlane) string {
	if lane.Name != "" {
		return lane.Name
	}
	return lane.Project
}

// calibrationSpanName identifies a subagent span by the kind of agent it ran,
// which is what makes a 10-hour span obviously wrong at a glance.
func calibrationSpanName(sp SubagentSpan) string {
	if sp.AgentType != "" {
		return sp.AgentType
	}
	return sp.AgentID
}
