package main

import (
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
