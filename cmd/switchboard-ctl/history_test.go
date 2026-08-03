package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
)

// evTime is a fixed instant for formatEvent cases. The rendered line uses
// ev.Ts.Local(), so the assertions match substrings of the detail/id/suffix
// rather than the (timezone-dependent) time prefix.
func evTime() time.Time {
	return time.Date(2026, 6, 26, 14, 32, 7, 0, time.UTC)
}

func TestFormatEvent(t *testing.T) {
	cases := []struct {
		name        string
		ev          history.Event
		wantContain []string
		wantOmit    []string
	}{
		{
			name: "transition with subagents, prev-duration, and rule suffix",
			ev: history.Event{
				Ts: evTime(), Type: history.EventTransition, SessionID: "ce13c0f2deadbeef",
				From: "permission", To: "working", Subagents: 2, DurPrevMs: 2000,
				Rule: "case9-approve-toolmatch", Project: "sb",
			},
			wantContain: []string{"transition", "ce13c0f2", "permission->working", "S=2", "sb", "2s", "(case9-approve-toolmatch)"},
			wantOmit:    []string{"ce13c0f2d"}, // 9th char of the session id must be truncated away
		},
		{
			name: "subagent_spawn renders type: description",
			ev: history.Event{
				Ts: evTime(), Type: history.EventSubagentSpawn, SessionID: "s1",
				AgentType: "Explore", Description: "map the auth code",
			},
			wantContain: []string{"subagent_spawn", "Explore: map the auth code"},
		},
		{
			name: "subagent_stop renders just the agent type",
			ev: history.Event{
				Ts: evTime(), Type: history.EventSubagentStop, SessionID: "s1", AgentType: "Explore",
			},
			wantContain: []string{"subagent_stop", "Explore"},
		},
		{
			name: "usage_sample renders in/out/combined-cache",
			ev: history.Event{
				Ts: evTime(), Type: history.EventUsageSample, SessionID: "s1",
				TokIn: 120, TokOut: 34, TokCacheRead: 1000, TokCacheCreate: 500,
			},
			wantContain: []string{"usage_sample", "in=120 out=34 cache=1500"},
		},
		{
			name: "no session id falls back to pidN",
			ev: history.Event{
				Ts: evTime(), Type: history.EventTransition, PID: 4242, From: "idle", To: "working",
			},
			wantContain: []string{"pid4242", "idle->working"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatEvent(c.ev)
			for _, want := range c.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("formatEvent = %q, missing %q", got, want)
				}
			}
			for _, omit := range c.wantOmit {
				if strings.Contains(got, omit) {
					t.Errorf("formatEvent = %q, should not contain %q", got, omit)
				}
			}
		})
	}
}

func TestOrDash(t *testing.T) {
	if got := orDash(""); got != "·" {
		t.Errorf(`orDash("") = %q, want "·"`, got)
	}
	if got := orDash("working"); got != "working" {
		t.Errorf(`orDash("working") = %q, want passthrough`, got)
	}
}

// The calibration report is the whole point of the subcommand: whatever it prints
// has to be enough to argue with, so both band endpoints must be attributable to a
// day-file and a session, and the two constants must be named as the things being
// scored.
func TestRenderCalibration(t *testing.T) {
	cal := history.Calibration{
		Dir:   "/tmp/history",
		Days:  []string{"2026-07-01", "2026-07-31"},
		Lanes: 651,
		LaneLegit: history.Population{Samples: []history.CalibrationSample{
			{Day: "2026-07-03", SessionID: "aaaaaaaabbbb", PID: 11, Name: "sb", Dur: time.Minute},
			{Day: "2026-07-14", SessionID: "ccccccccdddd", PID: 22, Name: "arachne", Dur: 2*time.Hour + 25*time.Minute + 21*time.Second},
		}},
		LaneGhost: history.Population{Samples: []history.CalibrationSample{
			{Day: "2026-07-22", SessionID: "eeeeeeeeffff", PID: 33, Name: "ghosted", Dur: 8*time.Hour + 57*time.Minute + 58*time.Second},
		}},
	}

	got := renderCalibrationString(t, cal)
	for _, want := range []string{
		"2 complete day(s): 2026-07-01 … 2026-07-31; 651 lane(s) replayed",
		"empty band 2h25m21s … 8h57m58s (6h32m37s wide)",
		"DefaultSuspectTrailingCap 4h0m0s",
		"DefaultSuspectSubagentCap 2h0m0s",
		"false positives  0",
		"false negatives  0",
		// The band endpoints, traceable back to the day-file they came from.
		"2026-07-14  cccccccc  pid 22",
		"2026-07-22  eeeeeeee  pid 33",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderCalibrationFlagsAMiscalibratedCap(t *testing.T) {
	t.Run("should report a false positive when a legitimate lane reaches the cap", func(t *testing.T) {
		cal := history.Calibration{
			Dir:  "/tmp/history",
			Days: []string{"2026-07-01"},
			LaneLegit: history.Population{Samples: []history.CalibrationSample{
				{Day: "2026-07-01", SessionID: "overrun", PID: 44, Dur: 6 * time.Hour},
			}},
			LaneGhost: history.Population{Samples: []history.CalibrationSample{
				{Day: "2026-07-01", SessionID: "ghost", PID: 55, Dur: 9 * time.Hour},
			}},
		}

		got := renderCalibrationString(t, cal)
		if !strings.Contains(got, "false positives  1 legitimate sample(s) at or above it") {
			t.Errorf("report should name the false positive:\n%s", got)
		}
	})

	t.Run("should say so when the two populations no longer separate", func(t *testing.T) {
		cal := history.Calibration{
			Dir:  "/tmp/history",
			Days: []string{"2026-07-01"},
			LaneLegit: history.Population{Samples: []history.CalibrationSample{
				{Day: "2026-07-01", SessionID: "long", PID: 44, Dur: 10 * time.Hour},
			}},
			LaneGhost: history.Population{Samples: []history.CalibrationSample{
				{Day: "2026-07-01", SessionID: "short-ghost", PID: 55, Dur: 3 * time.Hour},
			}},
		}

		got := renderCalibrationString(t, cal)
		if !strings.Contains(got, "no empty band") {
			t.Errorf("report should refuse to invent a band from overlapping populations:\n%s", got)
		}
	})
}

func renderCalibrationString(t *testing.T, cal history.Calibration) string {
	t.Helper()
	var buf bytes.Buffer
	renderCalibration(&buf, cal)
	return buf.String()
}
