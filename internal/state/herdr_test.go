package state

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// graphSession is a Claude session whose provider graph reports status.
func graphSession(status string, since time.Time) *Session {
	s := &Session{PID: 10, Agent: AgentKindClaude, Claude: &AgentInfo{Status: status, StatusSince: since}}
	s.AgentGraph = &AgentGraph{RootID: "sess-1", Summary: AgentGraphSummary{Status: status, Since: since}}
	return s
}

var herdrT0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func TestHerdrLegacyStatusShouldMapEveryHerdrState(t *testing.T) {
	for _, tc := range []struct {
		herdr, provider, want string
		ok                    bool
	}{
		{HerdrWorking, StatusIdle, StatusWorking, true},
		{HerdrBlocked, StatusWorking, StatusPermission, true},
		{HerdrIdle, StatusWorking, StatusIdle, true},
		{HerdrDone, StatusWorking, StatusIdle, true},
		{HerdrIdle, StatusDelegating, StatusDelegating, true},
		{HerdrDone, StatusDelegating, StatusDelegating, true},
		{HerdrWorking, StatusDelegating, StatusWorking, true},
		{HerdrUnknown, StatusWorking, "", false},
		{"", StatusWorking, "", false},
		{"sleeping", StatusWorking, "", false},
	} {
		got, ok := HerdrLegacyStatus(tc.herdr, tc.provider)
		if got != tc.want || ok != tc.ok {
			t.Errorf("HerdrLegacyStatus(%q, %q) = %q, %v; want %q, %v", tc.herdr, tc.provider, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSetHerdrShouldOverrideTheProviderStatusWhenHerdrIsLive(t *testing.T) {
	s := graphSession(StatusIdle, herdrT0)
	now := herdrT0.Add(time.Minute)

	before, after := s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrBlocked, true, now)
	if before != StatusIdle || after != StatusPermission {
		t.Fatalf("SetHerdr = %q → %q, want idle → permission", before, after)
	}
	if s.Claude.Status != StatusPermission || !s.Claude.StatusSince.Equal(now) {
		t.Fatalf("claude block = %q since %v, want permission since %v", s.Claude.Status, s.Claude.StatusSince, now)
	}
	if s.AgentGraph.Summary.Status != StatusIdle {
		t.Fatalf("graph summary = %q, want the provider's own idle kept", s.AgentGraph.Summary.Status)
	}
}

func TestSetHerdrShouldHandTheStatusBackToTheProviderWhenHerdrStopsBeingLive(t *testing.T) {
	s := graphSession(StatusIdle, herdrT0)
	s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrWorking, true, herdrT0.Add(time.Minute))

	before, after := s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrWorking, false, herdrT0.Add(2*time.Minute))
	if before != StatusWorking || after != StatusIdle {
		t.Fatalf("SetHerdr = %q → %q, want working → idle (the provider's)", before, after)
	}
	if !s.Claude.StatusSince.Equal(herdrT0) {
		t.Fatalf("StatusSince = %v, want the provider's own since %v", s.Claude.StatusSince, herdrT0)
	}
}

func TestSetHerdrShouldLeaveTheProviderStatusWhenHerdrReportsUnknown(t *testing.T) {
	s := graphSession(StatusWorking, herdrT0)
	before, after := s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrUnknown, true, herdrT0.Add(time.Minute))
	if before != StatusWorking || after != StatusWorking {
		t.Fatalf("SetHerdr = %q → %q, want working unchanged", before, after)
	}
}

func TestSetHerdrShouldNotMoveStatusSinceWhenThePublishedStatusIsUnchanged(t *testing.T) {
	s := graphSession(StatusWorking, herdrT0)
	s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrWorking, true, herdrT0.Add(time.Minute))
	if !s.Claude.StatusSince.Equal(herdrT0) {
		t.Fatalf("StatusSince = %v, want %v kept: working was already published", s.Claude.StatusSince, herdrT0)
	}
}

func TestSetHerdrShouldLeaveASessionWithoutAProviderGraphUnchanged(t *testing.T) {
	s := &Session{PID: 10, Agent: AgentKindClaude}
	before, after := s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrWorking, true, herdrT0)
	if before != "" || after != "" || s.Claude != nil {
		t.Fatalf("SetHerdr = %q → %q, claude=%+v; want nothing published", before, after, s.Claude)
	}
	if s.Herdr == nil || s.Herdr.Status != HerdrWorking {
		t.Fatalf("Herdr = %+v, want the pane recorded anyway", s.Herdr)
	}
}

// A provider observation landing while herdr is the authority must not paint
// over herdr's status.
func TestSetAgentGraphShouldKeepHerdrsStatusWhenAProviderObservationLands(t *testing.T) {
	s := graphSession(StatusIdle, herdrT0)
	s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrWorking, true, herdrT0.Add(time.Minute))

	s.SetAgentGraph(&AgentGraph{RootID: "sess-1", Summary: AgentGraphSummary{Status: StatusPermission, Since: herdrT0.Add(2 * time.Minute)}})
	if s.Claude.Status != StatusWorking {
		t.Fatalf("status = %q, want herdr's working to hold", s.Claude.Status)
	}
	if s.AgentGraph.Summary.Status != StatusPermission {
		t.Fatalf("graph summary = %q, want the provider's permission recorded", s.AgentGraph.Summary.Status)
	}
}

func TestSetAgentGraphShouldKeepDelegatingWhenHerdrSeesAnIdlePrompt(t *testing.T) {
	s := graphSession(StatusIdle, herdrT0)
	s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrIdle, true, herdrT0.Add(time.Minute))

	s.SetAgentGraph(&AgentGraph{RootID: "sess-1", Summary: AgentGraphSummary{Status: StatusDelegating, Since: herdrT0.Add(2 * time.Minute)}})
	if s.Claude.Status != StatusDelegating {
		t.Fatalf("status = %q, want delegating: herdr cannot see background agents", s.Claude.Status)
	}
}

// After a restart the persisted herdr status is stale until the watcher
// confirms it; only the wire fields survive, and they carry no authority.
func TestHerdrShouldCarryNoAuthorityWhenHydratedFromJSON(t *testing.T) {
	live := graphSession(StatusIdle, herdrT0)
	live.SetHerdr("w1:p1", "/h.sock", "claude", HerdrBlocked, true, herdrT0)
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"herdr":{"pane_id":"w1:p1","socket":"/h.sock","agent":"claude","status":"blocked"}`) {
		t.Fatalf("wire form = %s, want the herdr block", raw)
	}
	var restored Session
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	restored.SetAgentGraph(&AgentGraph{RootID: "sess-1", Summary: AgentGraphSummary{Status: StatusIdle}})
	if restored.Claude.Status != StatusIdle {
		t.Fatalf("status = %q, want the provider's idle: a hydrated herdr block is not live", restored.Claude.Status)
	}
}

func TestSnapshotShouldDetachTheHerdrBlockWhenTheStoreChangesLater(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(m map[int]*Session) {
		s := graphSession(StatusIdle, herdrT0)
		s.SetHerdr("w1:p1", "/h.sock", "claude", HerdrWorking, true, herdrT0)
		m[s.PID] = s
	})
	snap := store.Snapshot()
	store.Apply(func(m map[int]*Session) { m[10].Herdr.Status = HerdrBlocked })
	if got := snap.Sessions[0].Herdr.Status; got != HerdrWorking {
		t.Fatalf("snapshot herdr status = %q, want working: the snapshot shares the live block", got)
	}
}
