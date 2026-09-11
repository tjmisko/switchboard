package sessionview

import (
	"reflect"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

func TestPrepareSharesStatusFlagsAndActionIdentity(t *testing.T) {
	snapshot := state.Snapshot{Sessions: []state.Session{
		{PID: 11, Focused: true, Agent: state.AgentKindCodex, Codex: &state.AgentInfo{Status: state.StatusDelegating}},
		{PID: 12, Hostname: "other", Remote: true, Navigable: true, Suspended: true},
		{PID: 13, Hostname: "offline", Remote: true},
		{PID: 14, Headless: true},
	}}

	chips := Prepare(snapshot, 0)
	if got, want := chips[0].Classes, []string{"working", "delegating", "focused"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("local classes = %v, want %v", got, want)
	}
	if chips[0].Selector != "pid:11" {
		t.Fatalf("local selector = %q", chips[0].Selector)
	}
	if got, want := chips[1].Classes, []string{"unknown", "suspended", "remote"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remote classes = %v, want %v", got, want)
	}
	if chips[1].Selector != "host:other:pid:12" {
		t.Fatalf("remote selector = %q", chips[1].Selector)
	}
	if chips[2].Selector != "" || chips[3].Selector != "" {
		t.Fatalf("non-navigable selectors = %q, %q", chips[2].Selector, chips[3].Selector)
	}
	if got, want := chips[2].Classes, []string{"unknown", "remote", "unnavigable"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("offline classes = %v, want %v", got, want)
	}
}

func TestPrepareHonorsLimit(t *testing.T) {
	snapshot := state.Snapshot{Sessions: []state.Session{{PID: 1}, {PID: 2}, {PID: 3}}}
	if got := len(Prepare(snapshot, 2)); got != 2 {
		t.Fatalf("limited chips = %d, want 2", got)
	}
	if got := len(Prepare(snapshot, 0)); got != 3 {
		t.Fatalf("unlimited chips = %d, want 3", got)
	}
}
