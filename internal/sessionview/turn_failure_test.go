package sessionview

import (
	"slices"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

func TestPrepareShouldPaintAnErrorSessionRedAndTagItError(t *testing.T) {
	snap := state.Snapshot{Sessions: []state.Session{{PID: 1, Codex: &state.AgentInfo{Status: state.StatusError}, Agent: state.AgentKindCodex}}}
	item := Prepare(snap, 0)[0]
	if item.Status != state.StatusError {
		t.Fatalf("status = %q, want error", item.Status)
	}
	if !slices.Contains(item.Classes, "permission") || !slices.Contains(item.Classes, "error") {
		t.Fatalf("classes = %v, want the permission colour class plus error", item.Classes)
	}
}

func TestFailureDetailShouldNameTheKindWhenTheTurnFailed(t *testing.T) {
	for _, tc := range []struct {
		failure *state.TurnFailure
		want    string
	}{
		{&state.TurnFailure{Kind: state.TurnFailureUnreachable}, "can't reach API"},
		{&state.TurnFailure{Kind: state.TurnFailureAuth}, "auth failed"},
		{&state.TurnFailure{Kind: "other"}, "turn failed"},
		{nil, "turn failed"},
	} {
		if got := FailureDetail(state.Session{TurnFailure: tc.failure}); got != tc.want {
			t.Errorf("FailureDetail(%+v) = %q, want %q", tc.failure, got, tc.want)
		}
	}
}
