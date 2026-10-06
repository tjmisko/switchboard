package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
)

// writePiSessionFile writes a Pi session file whose newest assistant message
// stopped for stopReason, followed by a tool result when it called a tool.
func writePiSessionFile(t *testing.T, path, stopReason string) {
	t.Helper()
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	text := `{"type":"session","version":3,"id":"` + piTestSession + `","timestamp":"2026-10-05T12:00:00Z","cwd":"/project"}` + "\n" +
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-10-05T12:00:00Z","message":{"role":"user","content":"x"}}` + "\n" +
		fmt.Sprintf(`{"type":"message","id":"a1","parentId":"u1","timestamp":%q,"message":{"role":"assistant","content":[],"stopReason":%q}}`+"\n",
			at.Add(time.Second).Format(time.RFC3339Nano), stopReason)
	if stopReason == "toolUse" {
		text += `{"type":"message","id":"r1","parentId":"a1","timestamp":"2026-10-05T12:00:02Z","message":{"role":"toolResult","toolCallId":"c1","content":[]}}` + "\n"
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// restoredPiHarness is a Pi harness whose session carries the Pi block a
// daemon restart restores (session id, transcript, last status) and no
// reducer state: no hook has reached this daemon yet.
func restoredPiHarness(t *testing.T, stopReason string) (*piHarness, string) {
	t.Helper()
	h := newPiHarness(t)
	path := filepath.Join(t.TempDir(), "2026-10-05T12-00-00-000Z_"+piTestSession+".jsonl")
	writePiSessionFile(t, path, stopReason)
	h.store.Apply(func(sessions map[int]*state.Session) {
		sessions[piTestPID].Pi = &state.AgentInfo{SessionID: piTestSession, Transcript: path, Status: state.StatusIdle}
	})
	return h, path
}

func TestPiRestartShouldSeedIdleOrWorkingFromTheSessionFileTailWithoutLiveAuthority(t *testing.T) {
	h, path := restoredPiHarness(t, "toolUse")
	h.c.reconcilePiRoots(h.base)
	h.wantStatus("tail ends on a tool call", state.StatusWorking)
	graph := h.session().AgentGraph
	if graph == nil || graph.Source != agentgraph.SourcePiSessionFile || graph.RootID != piTestSession {
		t.Fatalf("seed graph = %+v, want a pi_session_file graph rooted at the session", graph)
	}
	if !graph.FreshUntil.Equal(h.base.Add(piSessionFileLease)) {
		t.Fatalf("seed lease ends %v, want the transcript window %v", graph.FreshUntil, h.base.Add(piSessionFileLease))
	}

	writePiSessionFile(t, path, "stop")
	later := h.base.Add(5 * time.Second)
	h.c.reconcilePiRoots(later)
	h.wantStatus("tail ends on a finished turn", state.StatusIdle)

	// Without a readable file the read is not renewed: it lapses at its lease.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.c.reconcilePiRoots(later.Add(piSessionFileLease - time.Millisecond))
	h.wantStatus("inside the lease", state.StatusIdle)
	h.c.reconcilePiRoots(later.Add(piSessionFileLease))
	h.wantStatus("lease lapsed, no herdr", "")

	var seeded bool
	for _, event := range h.events() {
		seeded = seeded || (event.Type == history.EventTransition && event.Rule == statustune.RulePiSessionFileRead)
	}
	if !seeded {
		t.Fatal("the session-file edge was not recorded as a transition")
	}
}

func TestPiRestartShouldRenewAnUnchangedSessionFileReadOnlyAfterHalfItsLease(t *testing.T) {
	h, _ := restoredPiHarness(t, "stop")
	h.c.reconcilePiRoots(h.base)
	first := h.session().AgentGraph.FreshUntil
	h.c.reconcilePiRoots(h.base.Add(piSessionFileLease/2 - time.Millisecond))
	if got := h.session().AgentGraph.FreshUntil; !got.Equal(first) {
		t.Fatalf("an unchanged file republished early: lease %v, want %v", got, first)
	}
	renewedAt := h.base.Add(piSessionFileLease / 2)
	h.c.reconcilePiRoots(renewedAt)
	if got := h.session().AgentGraph.FreshUntil; !got.Equal(renewedAt.Add(piSessionFileLease)) {
		t.Fatalf("lease after half its window = %v, want renewed to %v", got, renewedAt.Add(piSessionFileLease))
	}
}

func TestPiRestartShouldHandAuthorityToTheNextHook(t *testing.T) {
	h, _ := restoredPiHarness(t, "toolUse")
	h.c.reconcilePiRoots(h.base)
	h.wantStatus("seeded", state.StatusWorking)

	h.hook("Stop", time.Second)
	h.wantStatus("next hook", state.StatusIdle)
	if source := h.session().AgentGraph.Source; source != agentgraph.SourceHook {
		t.Fatalf("graph source after the hook = %q, want hook", source)
	}
	// The file still ends on a tool call, but the hook owns the root now.
	h.c.reconcilePiRoots(h.base.Add(2 * time.Second))
	h.wantStatus("reconcile after the hook", state.StatusIdle)
}

func TestPiRestartShouldHandAuthorityToALiveHerdrReading(t *testing.T) {
	h, _ := restoredPiHarness(t, "toolUse")
	h.c.reconcilePiRoots(h.base)
	h.wantStatus("seeded", state.StatusWorking)

	at := h.base.Add(time.Second)
	h.store.Apply(func(sessions map[int]*state.Session) {
		sessions[piTestPID].SetHerdr(state.HerdrReading{
			PaneID: "w1:p1", Socket: "/s", TerminalID: "term_1", Agent: state.AgentKindPi,
			Status: state.HerdrIdle, Live: true, Since: at,
		}, at)
	})
	h.wantStatus("herdr reading", state.StatusIdle)
	h.c.reconcilePiRoots(at.Add(time.Second))
	h.wantStatus("reconcile under live herdr", state.StatusIdle)
	if source := h.session().AgentGraph.Source; source != agentgraph.SourceHerdr {
		t.Fatalf("graph source under live herdr = %q, want herdr (the file must not be re-read)", source)
	}
}

func TestPiRestartShouldLeaveTheRestoredStatusWhenTheTailHoldsNoEvidence(t *testing.T) {
	h, path := restoredPiHarness(t, "stop")
	if err := os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.c.reconcilePiRoots(h.base)
	if graph := h.session().AgentGraph; graph != nil {
		t.Fatalf("a tail with no assistant message landed a graph: %+v", graph)
	}
}
