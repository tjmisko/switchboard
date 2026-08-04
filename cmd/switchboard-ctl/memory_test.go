package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
)

// The contract test for `memory --json`.
//
// This is the surface the dashboard codes against (docs/history-schema.md,
// "Memory"), so it pins FIELD NAMES AND UNITS, not just arithmetic: a rename here
// is a silent break in a consumer that lives in another repo. The fixture is an
// in-code event slice written to a temp day-file, matching every other fixture in
// this pipeline — there are no golden files and no testdata JSONL anywhere in it.
//
// One synthetic day carries every shape the contract has to get right:
//
//   - a tree that balloons and is reclaimed, so peak is not the last reading;
//   - unevenly spaced samples, so a naive mean and the time-weighted average
//     give visibly different answers;
//   - a session whose tree equals its agent, so `tree - agent` derives to zero;
//   - a nonzero swap component, the OOM-forensics case: a page pushed out is
//     still memory the session is answerable for, and dropping it is exactly what
//     loses the evidence;
//   - a suspect lane, whose series must stop where the timeline stopped believing
//     it rather than stretch to the bound;
//   - a tick with no PSI, whose fields must be ABSENT rather than reading as zero
//     stall — "not measured" and "measured, and idle" are different answers;
//   - sys_* repeated across two sessions in one tick, folding to one point;
//   - psi_stall_us as the delta between adjacent points, never the raw monotonic
//     since-boot counter.
const MiB = 1 << 20

const (
	sidBalloon = "aaaa1111-2222-4333-8444-555555555555" // tree balloons, then is reclaimed
	sidSolo    = "bbbb2222-3333-4444-8555-666666666666" // tree == agent throughout
	sidGhost   = "cccc3333-4444-4555-8666-777777777777" // silent lane the post-check flags
)

// memoryFixture writes the synthetic day and returns the dir and day key. The day
// is a past one, so the window ends at a local midnight rather than at wall-clock
// `now` and every derived figure is deterministic.
func memoryFixture(t *testing.T) (dir, day string) {
	t.Helper()
	dir = t.TempDir()
	d := time.Now().AddDate(0, 0, -7)
	day = d.Format("2006-01-02")
	at := func(h, m int) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, time.Local)
	}
	// sample builds one memory_sample. Bytes are split Pss/Swap deliberately: the
	// contract's `agent` and `tree` are Pss + SwapPss, and a fixture that only ever
	// set Pss would pass just as well if swap were dropped on the floor.
	sample := func(ts time.Time, sid string, pid int, project string,
		agentPss, agentSwap, treePss, treeSwap int64, procs int) history.Event {
		return history.Event{
			Ts: ts, Type: history.EventMemorySample, SessionID: sid, PID: pid,
			Agent: "claude", Project: project,
			MemAgentPssBytes: agentPss, MemAgentSwapBytes: agentSwap,
			MemTreePssBytes: treePss, MemTreeSwapBytes: treeSwap, MemTreeProcs: procs,
		}
	}
	// pressure stamps the machine-wide reading a tick carries. It is read once per
	// tick and copied onto every session's sample in it, which is what the fold has
	// to deduplicate.
	pressure := func(ev history.Event, availMiB int64, avg10 float64, totalUs int64) history.Event {
		ev.SysAvailBytes = availMiB * MiB
		ev.SysPsiSomeAvg10 = avg10
		ev.SysPsiSomeTotalUs = totalUs
		return ev
	}
	// availOnly is the same tick on a kernel built without CONFIG_PSI: MemAvailable
	// is readable, /proc/pressure/memory is not there at all.
	availOnly := func(ev history.Event, availMiB int64) history.Event {
		ev.SysAvailBytes = availMiB * MiB
		return ev
	}

	writeDay(t, dir, day,
		// --- the ghost: discovered, worked, last heard from at 09:30, never closed.
		history.Event{Ts: at(9, 0), Type: history.EventSessionStart, PID: 9001, Agent: "claude"},
		history.Event{Ts: at(9, 0), Type: history.EventTransition, PID: 9001, SessionID: sidGhost, To: "working"},
		// Its samples carry no sys_* at all — every field is omitempty, so a reader
		// has to tolerate a sample that contributes no pressure point.
		sample(at(9, 10), sidGhost, 9001, "ar", 200*MiB, 0, 200*MiB, 0, 1),
		sample(at(9, 20), sidGhost, 9001, "ar", 220*MiB, 0, 220*MiB, 0, 1),
		// The last real evidence. Token accrual IS work; the samples below are not.
		history.Event{Ts: at(9, 30), Type: history.EventUsageSample, PID: 9001, SessionID: sidGhost,
			Model: "claude-opus-4-8", TokIn: 1000, TokOut: 500},
		// A hung process still holds its pages, so the sampler keeps reporting long
		// after anything was heard from the session. These must be clipped — and the
		// figures are absurd on purpose, so a clip that silently fails is loud.
		sample(at(9, 40), sidGhost, 9001, "ar", 900*MiB, 0, 3000*MiB, 0, 12),
		sample(at(12, 0), sidGhost, 9001, "ar", 950*MiB, 0, 3200*MiB, 0, 14),

		// --- the balloon: a tree that grows tenfold under fanout, then is reclaimed.
		history.Event{Ts: at(9, 55), Type: history.EventSessionStart, PID: 4821, Agent: "claude"},
		history.Event{Ts: at(9, 55), Type: history.EventTransition, PID: 4821, SessionID: sidBalloon, To: "working"},
		pressure(sample(at(10, 0), sidBalloon, 4821, "sb", 400*MiB, 0, 400*MiB, 0, 1), 8000, 0.5, 1_000_000),
		pressure(sample(at(10, 10), sidBalloon, 4821, "sb", 400*MiB, 0, 2000*MiB, 0, 9), 6000, 2.5, 1_050_000),
		pressure(sample(at(10, 40), sidBalloon, 4821, "sb", 400*MiB, 0, 800*MiB, 0, 3), 7000, 0.5, 1_075_000),
		// …and a swap component on the last reading.
		pressure(sample(at(10, 50), sidBalloon, 4821, "sb", 400*MiB, 100*MiB, 800*MiB, 100*MiB, 3), 7200, 0.2, 1_075_000),

		// --- the solo session: no subagents, so the tree never exceeds the agent.
		history.Event{Ts: at(9, 58), Type: history.EventSessionStart, PID: 5102, Agent: "claude"},
		history.Event{Ts: at(9, 58), Type: history.EventTransition, PID: 5102, SessionID: sidSolo, To: "working"},
		// Same instant and same machine-wide reading as the balloon's first sample:
		// one tick, two sessions, one pressure point.
		pressure(sample(at(10, 0), sidSolo, 5102, "sbd", 300*MiB, 0, 300*MiB, 0, 1), 8000, 0.5, 1_000_000),
		pressure(sample(at(10, 30), sidSolo, 5102, "sbd", 350*MiB, 0, 350*MiB, 0, 1), 6500, 1.0, 1_070_000),
		// A kernel with no CONFIG_PSI writes neither PSI field.
		availOnly(sample(at(11, 0), sidSolo, 5102, "sbd", 300*MiB, 0, 300*MiB, 0, 1), 7500),

		history.Event{Ts: at(11, 30), Type: history.EventSessionEnd, PID: 4821, SessionID: sidBalloon},
		history.Event{Ts: at(11, 30), Type: history.EventSessionEnd, PID: 5102, SessionID: sidSolo},
	)
	return dir, day
}

// memoryDoc mirrors the frozen `memory --json` shape. The scalars are pointers so
// the test can tell a reported zero from an omitted field, and the series is
// decoded as raw maps where key PRESENCE is the assertion.
type memoryDoc struct {
	Window   string `json:"window"`
	Sessions []struct {
		SessionID      string `json:"session_id"`
		PID            int    `json:"pid"`
		Agent          string `json:"agent"`
		Project        string `json:"project"`
		PeakAgentBytes *int64 `json:"peak_agent_bytes"`
		AvgAgentBytes  *int64 `json:"avg_agent_bytes"`
		PeakTreeBytes  *int64 `json:"peak_tree_bytes"`
		AvgTreeBytes   *int64 `json:"avg_tree_bytes"`
		Mem            []struct {
			TS    time.Time `json:"ts"`
			Agent *int64    `json:"agent"`
			Tree  *int64    `json:"tree"`
		} `json:"mem"`
	} `json:"sessions"`
	Pressure []map[string]json.RawMessage `json:"pressure"`
}

func runMemoryJSON(t *testing.T, dir, day string) memoryDoc {
	t.Helper()
	out := captureStdout(t, func() {
		cmdMemory([]string{"--dir", dir, "--day", day, "--json"})
	})
	var doc memoryDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal memory document: %v\n%s", err, out)
	}
	return doc
}

func TestMemoryJSONContract(t *testing.T) {
	dir, day := memoryFixture(t)
	doc := runMemoryJSON(t, dir, day)

	if doc.Window != day {
		t.Errorf("window = %q, want %q", doc.Window, day)
	}
	// Ordered by first sample, then id — deterministic across map iteration.
	wantOrder := []string{sidGhost, sidBalloon, sidSolo}
	if len(doc.Sessions) != len(wantOrder) {
		t.Fatalf("got %d sessions, want %d", len(doc.Sessions), len(wantOrder))
	}
	for i, id := range wantOrder {
		if doc.Sessions[i].SessionID != id {
			t.Fatalf("sessions[%d] = %s, want %s (order is first-sample then id)", i, doc.Sessions[i].SessionID, id)
		}
	}
	byID := map[string]int{}
	for i, s := range doc.Sessions {
		byID[s.SessionID] = i
	}

	t.Run("should report the high-water mark rather than the last reading when a tree balloons and is reclaimed", func(t *testing.T) {
		s := doc.Sessions[byID[sidBalloon]]
		if s.PID != 4821 || s.Agent != "claude" || s.Project != "sb" {
			t.Errorf("identity = pid %d / %s / %s, want 4821 / claude / sb", s.PID, s.Agent, s.Project)
		}
		if got, want := *s.PeakTreeBytes, int64(2000*MiB); got != want {
			t.Errorf("peak_tree_bytes = %d, want the 10:10 balloon %d (not the %d it was reclaimed to)",
				got, want, 900*MiB)
		}
		if got, want := *s.PeakAgentBytes, int64(500*MiB); got != want {
			t.Errorf("peak_agent_bytes = %d, want %d", got, want)
		}
	})

	t.Run("should time-weight the averages over each sample's interval rather than take a mean of samples", func(t *testing.T) {
		s := doc.Sessions[byID[sidBalloon]]
		// Readings stand for 10m, 30m and 10m; the last closes the series and
		// carries no weight. (400·10 + 2000·30 + 800·10) / 50 = 1440 MiB.
		if got, want := *s.AvgTreeBytes, int64(1440*MiB); got != want {
			naive := int64((400 + 2000 + 800 + 900) / 4 * MiB)
			t.Errorf("avg_tree_bytes = %d, want the time-weighted %d (a mean of samples would be %d)", got, want, naive)
		}
		if got, want := *s.AvgAgentBytes, int64(400*MiB); got != want {
			t.Errorf("avg_agent_bytes = %d, want %d — the 500M reading closes the series and carries no weight", got, want)
		}
	})

	t.Run("should count swapped-out pages in both buckets", func(t *testing.T) {
		s := doc.Sessions[byID[sidBalloon]]
		if len(s.Mem) != 4 {
			t.Fatalf("mem has %d points, want 4", len(s.Mem))
		}
		last := s.Mem[3]
		// Pss 400M + SwapPss 100M. A page pushed out is still memory the session is
		// answerable for; reading Pss alone silently under-reports the two claude
		// processes on this machine carrying 154M and 120M of SwapPss.
		if got, want := *last.Agent, int64(500*MiB); got != want {
			t.Errorf("mem[3].agent = %d, want %d (Pss %d + SwapPss %d)", got, want, 400*MiB, 100*MiB)
		}
		if got, want := *last.Tree, int64(900*MiB); got != want {
			t.Errorf("mem[3].tree = %d, want %d (Pss %d + SwapPss %d)", got, want, 800*MiB, 100*MiB)
		}
	})

	t.Run("should derive spawned work to zero when a session's tree equals its agent", func(t *testing.T) {
		s := doc.Sessions[byID[sidSolo]]
		// `tree - agent` is the consumer's derivation, and it has to be exactly zero
		// for a session that never spawned: the two buckets are measured separately
		// and never subtracted from a third, so a shared page cannot leak into it.
		if got := *s.PeakTreeBytes - *s.PeakAgentBytes; got != 0 {
			t.Errorf("peak spawned = %d, want 0", got)
		}
		if got := *s.AvgTreeBytes - *s.AvgAgentBytes; got != 0 {
			t.Errorf("avg spawned = %d, want 0", got)
		}
		if got, want := *s.AvgTreeBytes, int64(325*MiB); got != want {
			t.Errorf("avg_tree_bytes = %d, want %d (300 and 350 each standing 30m)", got, want)
		}
	})

	t.Run("should stop a suspect lane's series where the evidence stopped rather than stretch it", func(t *testing.T) {
		s := doc.Sessions[byID[sidGhost]]
		if len(s.Mem) != 2 {
			t.Fatalf("mem has %d points, want the 2 that predate the 09:30 last evidence — %+v", len(s.Mem), s.Mem)
		}
		if got, want := s.Mem[1].TS.Hour()*60+s.Mem[1].TS.Minute(), 9*60+20; got != want {
			t.Errorf("last sample at %s, want 09:20 — the series ran past suspect_since", s.Mem[1].TS.Format("15:04"))
		}
		// The clipped readings were 3000M and 3200M; if the clip failed, the peak
		// says so immediately.
		if got, want := *s.PeakTreeBytes, int64(220*MiB); got != want {
			t.Errorf("peak_tree_bytes = %d, want %d — a post-suspect_since reading was folded in", got, want)
		}
	})

	t.Run("should fold the machine-wide reading to one point per tick", func(t *testing.T) {
		// Seven samples carry sys_* (four balloon, two solo with pressure, one solo
		// without PSI), but the balloon's and the solo's 10:00 readings are the same
		// tick observed twice. The ghost's samples carry no sys_* and contribute none.
		if len(doc.Pressure) != 6 {
			t.Fatalf("got %d pressure points, want 6 (the two 10:00 samples are one tick)\n%+v", len(doc.Pressure), doc.Pressure)
		}
		if got, want := rawInt(t, doc.Pressure[0], "avail_bytes"), int64(8000*MiB); got != want {
			t.Errorf("pressure[0].avail_bytes = %d, want %d", got, want)
		}
	})

	t.Run("should report psi_stall_us as the delta between adjacent points not the monotonic counter", func(t *testing.T) {
		// The raw sys_psi_some_total_us counter runs 1_000_000 → 1_050_000 →
		// 1_070_000 → 1_075_000 → 1_075_000. What the machine actually stalled in
		// each interval is the difference; the first point has no predecessor inside
		// the window and so reports 0.
		want := []int64{0, 50_000, 20_000, 5_000, 0}
		for i, w := range want {
			if got := rawInt(t, doc.Pressure[i], "psi_stall_us"); got != w {
				t.Errorf("pressure[%d].psi_stall_us = %d, want %d", i, got, w)
			}
		}
		if got, want := rawFloat(t, doc.Pressure[1], "psi_avg10"), 2.5; got != want {
			t.Errorf("pressure[1].psi_avg10 = %v, want %v", got, want)
		}
	})

	t.Run("should omit the psi fields entirely when the kernel did not measure them", func(t *testing.T) {
		last := doc.Pressure[5]
		for _, field := range []string{"psi_avg10", "psi_stall_us"} {
			if _, ok := last[field]; ok {
				t.Errorf("pressure[5] carries %q = %s; absent means NOT MEASURED and zero means measured-and-idle, and OOM forensics turns on the difference",
					field, last[field])
			}
		}
		// The reading it DID take is still there.
		if got, want := rawInt(t, last, "avail_bytes"), int64(7500*MiB); got != want {
			t.Errorf("pressure[5].avail_bytes = %d, want %d", got, want)
		}
	})
}

// The other half of the memory decision: memory rides its own surface so a live
// sample series never changes the timeline bytes, which is what the dashboard's
// unchanged→no-repaint check compares. Adding memory events to a day must leave
// `timeline --json` BYTE-FOR-BYTE identical.
func TestTimelineJSONIsUnaffectedByMemoryEvents(t *testing.T) {
	t.Run("should emit identical timeline bytes with and without the memory samples", func(t *testing.T) {
		withMem, day := memoryFixture(t)
		bare := t.TempDir()
		// The same day, stripped of every memory_sample.
		evs, err := history.ReadDay(withMem, day)
		if err != nil {
			t.Fatal(err)
		}
		var kept []history.Event
		for _, ev := range evs {
			if ev.Type != history.EventMemorySample {
				kept = append(kept, ev)
			}
		}
		writeDay(t, bare, day, kept...)

		run := func(dir string) string {
			return captureStdout(t, func() {
				cmdTimeline([]string{"--dir", dir, "--day", day, "--json"})
			})
		}
		if a, b := run(withMem), run(bare); a != b {
			t.Errorf("timeline --json differs once memory samples are recorded:\n--- with ---\n%s\n--- without ---\n%s", a, b)
		}
	})
}

func rawInt(t *testing.T, m map[string]json.RawMessage, field string) int64 {
	t.Helper()
	raw, ok := m[field]
	if !ok {
		t.Fatalf("field %q absent from %v", field, m)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("field %q = %s: %v", field, raw, err)
	}
	return v
}

func rawFloat(t *testing.T, m map[string]json.RawMessage, field string) float64 {
	t.Helper()
	raw, ok := m[field]
	if !ok {
		t.Fatalf("field %q absent from %v", field, m)
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("field %q = %s: %v", field, raw, err)
	}
	return v
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{1023, "1023B"}, // boundary: under 1K stays a plain count
		{1024, "1.0K"},  // boundary: 1K switches unit
		{1536, "1.5K"},
		{658374656, "627.9M"}, // the schema doc's tree reading, exactly 627.875 MiB
		{3 << 30, "3.0G"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
