package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

var explainT0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// explainAt explains pid as of at, as the RPC would with a snapshot taken then.
func explainAt(t *testing.T, c *agentCoordinator, pid int, at time.Time) statusexplain.Decision {
	t.Helper()
	snap := c.store.Snapshot()
	snap.UpdatedAt = at
	d, err := c.Explain(snap, pid)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func claudeExplainObservation(rootID string, source agentgraph.SourceKind, runtime agentgraph.RuntimeState, at time.Time, lease time.Duration) agentgraph.Observation {
	return agentgraph.Observation{
		Provider: agentgraph.ProviderClaude, RootID: rootID, Source: source,
		ObservedAt: at, FreshUntil: at.Add(lease),
		Nodes: []agentgraph.Node{{ID: rootID, Runtime: runtime, UpdatedAt: at}},
	}
}

// claudeExplainCoordinator is a coordinator over one bound Claude root whose
// transcript graph read idle at explainT0, fresh for a minute.
func claudeExplainCoordinator(t *testing.T) (*agentCoordinator, provider.RootRef) {
	t.Helper()
	store := state.New("")
	ref := seedCoordinatorSession(store, 610, explainT0.Add(-time.Hour), state.AgentKindClaude, "claude-root", "/repo")
	c := newAgentCoordinator(store, nil, nil, nil)
	t.Cleanup(c.Close)
	c.refreshTrackedRoots()
	if !c.applyObservation(ref, c.begin(ref.Key()),
		claudeExplainObservation("claude-root", agentgraph.SourceClaudeTranscript, agentgraph.RuntimeIdle, explainT0, time.Minute),
		claudeprovider.Compatibility{}, explainT0) {
		t.Fatal("transcript graph did not land")
	}
	return c, ref
}

func TestExplainShouldListASourceOutrankedCandidateAsRejectedWhenAFreshHigherRankedGraphHolds(t *testing.T) {
	c, ref := claudeExplainCoordinator(t)
	at := explainT0.Add(time.Second)
	hook := claudeExplainObservation("claude-root", agentgraph.SourceHook, agentgraph.RuntimeActive, at, time.Minute)
	// The hook is evidence of its own kind: it lands, and the resolver weighs
	// it against the fresh transcript graph, which outranks it.
	if !c.applyObservationAs(ref, c.begin(ref.Key()), hook, claudeprovider.Compatibility{}, at, "", state.GraphHookEvent) {
		t.Fatal("the hook's evidence did not land")
	}

	d := explainAt(t, c, ref.PID, at)
	if d.Status != state.StatusIdle || d.Reason != statusexplain.ReasonGraphAuthority ||
		d.Source != string(agentgraph.SourceClaudeTranscript) || !d.FreshUntil.Equal(explainT0.Add(time.Minute)) {
		t.Fatalf("decision = %+v, want the transcript's idle", d.Choice)
	}
	if len(d.Rejected) != 1 {
		t.Fatalf("rejected = %+v", d.Rejected)
	}
	if got := d.Rejected[0]; got.Source != string(agentgraph.SourceHook) || got.Status != state.StatusWorking ||
		got.RejectReason != statusexplain.ReasonSourceOutranked || !got.ObservedAt.Equal(at) {
		t.Fatalf("rejected = %+v, want the hook's working rejected as source_outranked", got)
	}
}

func TestExplainShouldBoundTheRejectedListWhenManyCandidatesArrive(t *testing.T) {
	c, ref := claudeExplainCoordinator(t)
	const arrivals = 3 * statusexplain.MaxRejected
	for i := range arrivals {
		// Each is older than the transcript graph its kind already holds, so the
		// landing path refuses it before it becomes evidence.
		at := explainT0.Add(-time.Duration(i+1) * time.Millisecond)
		old := claudeExplainObservation("claude-root", agentgraph.SourceClaudeTranscript, agentgraph.RuntimeActive, at, time.Minute)
		if c.applyObservation(ref, c.begin(ref.Key()), old, claudeprovider.Compatibility{}, explainT0) {
			t.Fatalf("an older transcript graph landed over a newer one")
		}
	}
	d := explainAt(t, c, ref.PID, explainT0.Add(time.Second))
	if len(d.Rejected) != statusexplain.MaxRejected || d.RejectedOmitted != arrivals-statusexplain.MaxRejected {
		t.Fatalf("rejected = %d omitted = %d, want %d and %d", len(d.Rejected), d.RejectedOmitted,
			statusexplain.MaxRejected, arrivals-statusexplain.MaxRejected)
	}
	last := explainT0.Add(-time.Duration(arrivals) * time.Millisecond)
	if got := d.Rejected[len(d.Rejected)-1]; !got.ObservedAt.Equal(last) || got.RejectReason != statusexplain.ReasonOlderThanCurrent {
		t.Fatalf("last rejected = %+v, want the last arrival refused as older_than_current", got)
	}
}

func TestExplainShouldStartAFreshRejectedListWhenANewGraphIsAdmitted(t *testing.T) {
	c, ref := claudeExplainCoordinator(t)
	c.applyObservation(ref, c.begin(ref.Key()),
		claudeExplainObservation("claude-root", agentgraph.SourceClaudeTranscript, agentgraph.RuntimeActive, explainT0.Add(-time.Second), time.Minute),
		claudeprovider.Compatibility{}, explainT0)
	if d := explainAt(t, c, ref.PID, explainT0); len(d.Rejected) != 1 {
		t.Fatalf("setup: rejected = %+v, want the older graph refused", d.Rejected)
	}
	later := explainT0.Add(2 * time.Second)
	if !c.applyObservation(ref, c.begin(ref.Key()),
		claudeExplainObservation("claude-root", agentgraph.SourceClaudeTranscript, agentgraph.RuntimeActive, later, time.Minute),
		claudeprovider.Compatibility{}, later) {
		t.Fatal("newer transcript graph did not land")
	}
	if d := explainAt(t, c, ref.PID, later); len(d.Rejected) != 0 || d.Status != state.StatusWorking {
		t.Fatalf("decision = %+v rejected = %+v, want working with nothing rejected against it", d.Choice, d.Rejected)
	}
}

func TestExplainShouldSayEventAuthorityWhenAHooksEventDecides(t *testing.T) {
	store := state.New("")
	ref := seedCoordinatorSession(store, 620, explainT0.Add(-time.Hour), state.AgentKindCodex, "thread-1", "/repo")
	c := newAgentCoordinator(store, nil, nil, nil)
	defer c.Close()
	c.refreshTrackedRoots()
	observation := testCodexObservation(ref, "thread-1", explainT0, agentgraph.RuntimeActive, agentgraph.AttentionNone)
	observation.Source = agentgraph.SourceHook
	observation.Complete = false
	if !c.applyObservationAs(ref, c.begin(ref.Key()), observation, claudeprovider.Compatibility{}, explainT0, "", state.GraphHookEvent) {
		t.Fatal("hook graph did not land")
	}
	d := explainAt(t, c, ref.PID, explainT0)
	if d.Status != state.StatusWorking || d.Reason != statusexplain.ReasonEventAuthority || d.Source != string(agentgraph.SourceHook) {
		t.Fatalf("decision = %+v, want working as event_authority", d.Choice)
	}

	// A newer app-server sample is a provider snapshot: graph authority.
	later := explainT0.Add(time.Second)
	sample := testCodexObservation(ref, "thread-1", later, agentgraph.RuntimeActive, agentgraph.AttentionNone)
	if !c.applyObservation(ref, c.begin(ref.Key()), sample, claudeprovider.Compatibility{}, later) {
		t.Fatal("app-server sample did not land")
	}
	if d := explainAt(t, c, ref.PID, later); d.Reason != statusexplain.ReasonGraphAuthority {
		t.Fatalf("decision = %+v, want graph_authority", d.Choice)
	}
}

// Acceptance criterion 2, through the coordinator's own observe path.
func TestExplainShouldGiveDistinctReasonsWhenBindingIsMissingObservationExpiredOrCoverageUnsupported(t *testing.T) {
	store := state.New("")
	started := explainT0.Add(-time.Hour)
	store.Apply(func(sessions map[int]*state.Session) {
		// Claude with no session id: nothing can bind it.
		sessions[631] = &state.Session{PID: 631, StartedAt: started, Agent: state.AgentKindClaude, CWD: "/a"}
		// Codex bound by a hook, with no observer to read it (-codex-observer off).
		sessions[632] = &state.Session{PID: 632, StartedAt: started, Agent: state.AgentKindCodex, CWD: "/b",
			Codex: &state.AgentInfo{SessionID: "thread-2"}}
		// Claude whose graph has passed its deadline.
		sessions[633] = &state.Session{PID: 633, StartedAt: started, Agent: state.AgentKindClaude, CWD: "/c",
			Claude: &state.AgentInfo{SessionID: "claude-3"}}
	})
	c := newAgentCoordinator(store, nil, claudeprovider.NewObserver(t.TempDir()), nil)
	defer c.Close()
	refs := c.refreshTrackedRoots()
	for _, ref := range refs {
		if ref.PID == 633 {
			c.applyObservation(ref, c.begin(ref.Key()),
				claudeExplainObservation("claude-3", agentgraph.SourceClaudeTranscript, agentgraph.RuntimeActive, explainT0, time.Minute),
				claudeprovider.Compatibility{}, explainT0)
		}
	}
	later := explainT0.Add(2 * time.Minute)
	for _, ref := range refs {
		if ref.PID == 633 {
			c.expireCurrent(ref, c.begin(ref.Key()), later)
			continue
		}
		c.observeAt(t.Context(), ref, later)
	}

	want := map[int]statusexplain.Reason{
		631: statusexplain.ReasonBindingMissing,
		632: statusexplain.ReasonCoverageUnsupported,
		633: statusexplain.ReasonObservationExpired,
	}
	for pid, reason := range want {
		d := explainAt(t, c, pid, later)
		if d.Reason != reason || d.Status != statusexplain.StatusUnknown {
			t.Errorf("pid %d: decision = %+v, want unknown for %s", pid, d.Choice, reason)
		}
	}
}

// Acceptance criterion 3, through the coordinator: the same PID and even the
// same conversation id, in a new process lifetime, explains afresh.
func TestExplainShouldNotExposeThePreviousRootsDecisionWhenTheProcessIsReplaced(t *testing.T) {
	c, ref := claudeExplainCoordinator(t)
	at := explainT0.Add(time.Second)
	c.applyObservationAs(ref, c.begin(ref.Key()),
		claudeExplainObservation("claude-root", agentgraph.SourceHook, agentgraph.RuntimeActive, at, time.Minute),
		claudeprovider.Compatibility{}, at, "", state.GraphHookEvent)
	if d := explainAt(t, c, ref.PID, at); len(d.Rejected) != 1 {
		t.Fatalf("setup: rejected = %+v", d.Rejected)
	}

	replacedAt := explainT0.Add(time.Hour)
	c.store.Apply(func(sessions map[int]*state.Session) {
		sessions[ref.PID] = &state.Session{PID: ref.PID, StartedAt: replacedAt, Agent: state.AgentKindClaude, CWD: "/repo",
			Claude: &state.AgentInfo{SessionID: "claude-root"}}
	})
	// Before any cleanup has run: the lifetime fence alone must hold.
	d := explainAt(t, c, ref.PID, replacedAt)
	if !d.Root.StartedAt.Equal(replacedAt) {
		t.Fatalf("root = %+v, want the new lifetime", d.Root)
	}
	if len(d.Rejected) != 0 || d.Reason == statusexplain.ReasonGraphAuthority || d.Source != "" {
		t.Fatalf("replaced process explained by the previous lifetime: %+v", d)
	}
	if d.Reason != statusexplain.ReasonObservationPending {
		t.Fatalf("reason = %q, want observation_pending", d.Reason)
	}

	c.refreshTrackedRoots()
	c.mu.Lock()
	_, kept := c.admissions[ref.Key()]
	c.mu.Unlock()
	if kept {
		t.Fatal("the previous lifetime's admission record survived its root")
	}
}

// Acceptance criterion 4, at the daemon's edge: whatever the session holds,
// the explanation carries ids, enums and times.
func TestExplainShouldCarryNoUserContentWhenTheSessionHoldsSome(t *testing.T) {
	c, ref := claudeExplainCoordinator(t)
	c.store.Apply(func(sessions map[int]*state.Session) {
		sess := sessions[ref.PID]
		sess.CWD = "/home/u/secret-repo"
		sess.DisplayName = &state.DisplayName{Value: "fix the secret bug", Origin: state.DisplayNameNative}
		sess.Claude.Transcript = "/home/u/.claude/projects/secret/x.jsonl"
		sess.Claude.PendingTool = "Bash"
	})
	d := explainAt(t, c, ref.PID, explainT0)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"secret", "/home/u", "Bash"} {
		if strings.Contains(string(raw), leaked) || strings.Contains(d.Text(), leaked) {
			t.Fatalf("explanation carries %q:\n%s", leaked, raw)
		}
	}
}

func TestExplainLocalShouldRefuseAnotherHostWhenAHostnameIsGiven(t *testing.T) {
	c, ref := claudeExplainCoordinator(t)
	if _, err := c.ExplainLocal("box-a", "box-b", ref.PID); err == nil {
		t.Fatal("explained a root on another host from local state")
	}
	for _, host := range []string{"", "box-a", "BOX-A."} {
		if _, err := c.ExplainLocal("box-a", host, ref.PID); err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
	}
	if _, err := c.ExplainLocal("box-a", "", 999999); err == nil {
		t.Fatal("explained a pid with no session")
	}
}

// Acceptance criterion 1 for Pi, through the real hook path.
func TestExplainShouldExplainAPiDialogsPermissionAndTheRejectedTerminalReadingWhenHerdrReadsWorking(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	h.hook("PermissionRequest", 2*time.Second, dialogs(1))
	at := h.base.Add(3 * time.Second)
	h.store.Apply(func(sessions map[int]*state.Session) {
		sessions[piTestPID].SetHerdr(state.HerdrReading{PaneID: "w1:p1", Socket: "/h.sock", TerminalID: "t1",
			Agent: "pi", Status: state.HerdrWorking, Live: true, Since: at}, at)
	})
	h.wantStatus("dialog open under herdr working", state.StatusPermission)

	d := explainAt(t, h.c, piTestPID, at)
	if d.Status != state.StatusPermission || d.Reason != statusexplain.ReasonEventAuthority ||
		d.Root.SessionID != piTestSession {
		t.Fatalf("decision = %+v root = %+v, want permission from the Pi hook", d.Choice, d.Root)
	}
	if len(d.Rejected) != 1 || d.Rejected[0].Status != state.StatusWorking ||
		d.Rejected[0].RejectReason != statusexplain.ReasonSourceOutranked {
		t.Fatalf("rejected = %+v, want herdr's working outranked by the Pi hook", d.Rejected)
	}
}

// A hook lease that lapses onto a herdr reading of the same colour changes no
// status, but it does change who decides; explain must follow it.
func TestExplainShouldFollowAPiDecisionToHerdrWhenTheHookLeaseLapsesWithoutAStatusChange(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	at := h.base.Add(2 * time.Second)
	h.store.Apply(func(sessions map[int]*state.Session) {
		sessions[piTestPID].SetHerdr(state.HerdrReading{PaneID: "w1:p1", Socket: "/h.sock", TerminalID: "t1",
			Agent: "pi", Status: state.HerdrWorking, Live: true, Since: at}, at)
	})
	if d := explainAt(t, h.c, piTestPID, at); d.Reason != statusexplain.ReasonEventAuthority {
		t.Fatalf("before the lapse: %+v", d.Choice)
	}
	lapsed := h.base.Add(time.Second + piHookActiveLease + time.Second)
	h.c.reconcilePiRoots(lapsed)
	h.wantStatus("lease lapsed onto herdr working", state.StatusWorking)
	d := explainAt(t, h.c, piTestPID, lapsed)
	if d.Reason != statusexplain.ReasonTerminalAuthority || d.Source != string(agentgraph.SourceHerdr) {
		t.Fatalf("after the lapse: %+v, want terminal_authority", d.Choice)
	}
}
