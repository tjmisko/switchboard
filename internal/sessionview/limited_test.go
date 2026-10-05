package sessionview

import (
	"slices"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

func TestPrepareShouldPaintALimitedSessionGreyAndTagItLimited(t *testing.T) {
	snap := state.Snapshot{Sessions: []state.Session{{PID: 1, Codex: &state.AgentInfo{Status: state.StatusLimited}, Agent: state.AgentKindCodex}}}
	item := Prepare(snap, 0)[0]
	if item.Status != state.StatusLimited {
		t.Fatalf("status = %q, want limited", item.Status)
	}
	if !slices.Contains(item.Classes, "unknown") || !slices.Contains(item.Classes, "limited") {
		t.Fatalf("classes = %v, want the unknown colour class plus limited", item.Classes)
	}
}

func TestLimitDetailShouldNameTheResetClockAndItsDayWhenNotToday(t *testing.T) {
	zone := time.FixedZone("PDT", -7*3600)
	now := time.Date(2026, 10, 5, 12, 28, 0, 0, zone)
	at := func(day, hour, minute int) *time.Time {
		v := time.Date(2026, 10, day, hour, minute, 0, 0, zone).UTC()
		return &v
	}
	for _, tc := range []struct {
		limit *state.UsageLimit
		want  string
	}{
		{&state.UsageLimit{ResetsAt: at(5, 17, 15)}, "usage limit · resets 5:15 PM"},
		{&state.UsageLimit{ResetsAt: at(8, 21, 0)}, "usage limit · resets Thu 9:00 PM"},
		{&state.UsageLimit{}, "usage limit"},
		{nil, "usage limit"},
	} {
		if got := LimitDetail(state.Session{UsageLimit: tc.limit}, now); got != tc.want {
			t.Errorf("LimitDetail(%+v) = %q, want %q", tc.limit, got, tc.want)
		}
	}
}
