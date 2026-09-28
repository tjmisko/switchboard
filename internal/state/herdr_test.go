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

// reading is a herdr reading of pane w1:p1 running claude.
func reading(status string, live bool, since time.Time) HerdrReading {
	return HerdrReading{PaneID: "w1:p1", Socket: "/h.sock", TerminalID: "term_1", Agent: "claude", Status: status, Live: live, Since: since}
}

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

	before, after := s.SetHerdr(reading(HerdrBlocked, true, now), now)
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
	s.SetHerdr(reading(HerdrWorking, true, herdrT0.Add(time.Minute)), herdrT0.Add(time.Minute))

	before, after := s.SetHerdr(reading(HerdrWorking, false, herdrT0.Add(2*time.Minute)), herdrT0.Add(2*time.Minute))
	if before != StatusWorking || after != StatusIdle {
		t.Fatalf("SetHerdr = %q → %q, want working → idle (the provider's)", before, after)
	}
	if !s.Claude.StatusSince.Equal(herdrT0) {
		t.Fatalf("StatusSince = %v, want the provider's own since %v", s.Claude.StatusSince, herdrT0)
	}
}

func TestSetHerdrShouldLeaveTheProviderStatusWhenHerdrReportsUnknown(t *testing.T) {
	s := graphSession(StatusWorking, herdrT0)
	before, after := s.SetHerdr(reading(HerdrUnknown, true, herdrT0.Add(time.Minute)), herdrT0.Add(time.Minute))
	if before != StatusWorking || after != StatusWorking {
		t.Fatalf("SetHerdr = %q → %q, want working unchanged", before, after)
	}
}

func TestSetHerdrShouldNotMoveStatusSinceWhenThePublishedStatusIsUnchanged(t *testing.T) {
	s := graphSession(StatusWorking, herdrT0)
	s.SetHerdr(reading(HerdrWorking, true, herdrT0.Add(time.Minute)), herdrT0.Add(time.Minute))
	if !s.Claude.StatusSince.Equal(herdrT0) {
		t.Fatalf("StatusSince = %v, want %v kept: working was already published", s.Claude.StatusSince, herdrT0)
	}
}

func TestSetHerdrShouldLeaveASessionWithoutAProviderGraphUnchanged(t *testing.T) {
	s := &Session{PID: 10, Agent: AgentKindClaude}
	before, after := s.SetHerdr(reading(HerdrWorking, true, herdrT0), herdrT0)
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
	s.SetHerdr(reading(HerdrWorking, true, herdrT0.Add(time.Minute)), herdrT0.Add(time.Minute))

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
	s.SetHerdr(reading(HerdrIdle, true, herdrT0.Add(time.Minute)), herdrT0.Add(time.Minute))

	s.SetAgentGraph(&AgentGraph{RootID: "sess-1", Summary: AgentGraphSummary{Status: StatusDelegating, Since: herdrT0.Add(2 * time.Minute)}})
	if s.Claude.Status != StatusDelegating {
		t.Fatalf("status = %q, want delegating: herdr cannot see background agents", s.Claude.Status)
	}
}

// After a restart the persisted herdr status is stale until the watcher
// confirms it; only the wire fields survive, and they carry no authority.
func TestHerdrShouldCarryNoAuthorityWhenHydratedFromJSON(t *testing.T) {
	live := graphSession(StatusIdle, herdrT0)
	live.SetHerdr(reading(HerdrBlocked, true, herdrT0), herdrT0)
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
		s.SetHerdr(reading(HerdrWorking, true, herdrT0), herdrT0)
		m[s.PID] = s
	})
	snap := store.Snapshot()
	store.Apply(func(m map[int]*Session) { m[10].Herdr.Status = HerdrBlocked })
	if got := snap.Sessions[0].Herdr.Status; got != HerdrWorking {
		t.Fatalf("snapshot herdr status = %q, want working: the snapshot shares the live block", got)
	}
}

func TestSetHerdrShouldGiveAHerdrOnlyAgentAOneNodeGraph(t *testing.T) {
	s := &Session{PID: 20, Agent: "opencode"}
	r := reading(HerdrWorking, true, herdrT0)
	r.Agent = "opencode"
	before, after := s.SetHerdr(r, herdrT0.Add(time.Second))
	if before != "" || after != StatusWorking {
		t.Fatalf("SetHerdr = %q → %q, want unknown → working", before, after)
	}
	g := s.AgentGraph
	if g == nil || g.RootID != "herdr:term_1" || g.Source != "herdr" || len(g.Nodes) != 1 {
		t.Fatalf("graph = %+v, want one herdr-sourced node rooted at the terminal id", g)
	}
	if s.Claude != nil || s.Codex != nil {
		t.Fatal("a herdr-only agent grew a claude/codex enrichment block")
	}
}

func TestSetHerdrShouldExpireAHerdrOnlyAgentsGraphWhenHerdrIsLost(t *testing.T) {
	s := &Session{PID: 20, Agent: "pi"}
	s.SetHerdr(reading(HerdrBlocked, true, herdrT0), herdrT0.Add(time.Second))

	before, after := s.SetHerdr(reading(HerdrBlocked, false, herdrT0), herdrT0.Add(time.Minute))
	if before != StatusPermission || after != "" {
		t.Fatalf("SetHerdr = %q → %q, want permission → unknown", before, after)
	}
}

func TestSetHerdrShouldKeepAHerdrOnlyAgentsRootWhenItsPaneMoves(t *testing.T) {
	s := &Session{PID: 20, Agent: "pi"}
	s.SetHerdr(reading(HerdrIdle, true, herdrT0), herdrT0)
	root := s.AgentGraph.RootID

	moved := reading(HerdrIdle, true, herdrT0)
	moved.PaneID = "w3:p1"
	s.SetHerdr(moved, herdrT0.Add(time.Minute))
	if s.AgentGraph.RootID != root || s.Herdr.PaneID != "w3:p1" {
		t.Fatalf("root %q pane %q, want root %q kept across the move", s.AgentGraph.RootID, s.Herdr.PaneID, root)
	}
}

// At startup the watcher has not reconnected yet: its first reading of a
// hydrated pane is "not live" with no status. That must not flash the chip.
func TestSetHerdrShouldKeepAHydratedStatusWhenTheFirstReadingIsNotLive(t *testing.T) {
	s := &Session{PID: 20, Agent: "pi"}
	s.SetHerdr(reading(HerdrIdle, true, herdrT0), herdrT0)
	s.Herdr.Live = false // what hydration from state.json leaves
	root := s.AgentGraph.RootID

	before, after := s.SetHerdr(HerdrReading{PaneID: "w1:p1", Socket: "/h.sock"}, herdrT0.Add(time.Minute))
	if before != StatusIdle || after != StatusIdle || s.AgentGraph.RootID != root {
		t.Fatalf("SetHerdr = %q → %q root %q, want idle kept under root %q", before, after, s.AgentGraph.RootID, root)
	}

	claude := graphSession(StatusWorking, herdrT0)
	if before, after := claude.SetHerdr(HerdrReading{PaneID: "w1:p1", Socket: "/h.sock"}, herdrT0); before != after {
		t.Fatalf("claude SetHerdr = %q → %q, want no change", before, after)
	}
}

func TestSetHerdrShouldKeepTheRootWhenAReadingHasNoTerminalID(t *testing.T) {
	s := &Session{PID: 20, Agent: "pi"}
	s.SetHerdr(reading(HerdrIdle, true, herdrT0), herdrT0)
	root := s.AgentGraph.RootID

	r := reading(HerdrWorking, true, herdrT0.Add(time.Second))
	r.TerminalID = ""
	s.SetHerdr(r, herdrT0.Add(time.Second))
	if s.AgentGraph.RootID != root || s.AgentGraph.Summary.Status != StatusWorking {
		t.Fatalf("root %q status %q, want working under root %q", s.AgentGraph.RootID, s.AgentGraph.Summary.Status, root)
	}
}
