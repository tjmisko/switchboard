package statusresolve

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// Every test runs on a fixed clock: t0 is "now" unless a test moves it.
var (
	t0       = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	lifetime = t0.Add(-time.Hour) // the tracked agent process's start
)

const sessionID = "sess-1"

// observation is a one-root graph of provider from source at at, fresh for
// lease, with optional children.
func observation(provider agentgraph.ProviderKind, source agentgraph.SourceKind, at time.Time, lease time.Duration,
	runtime agentgraph.RuntimeState, attention agentgraph.AttentionState, children ...agentgraph.Node) agentgraph.Observation {
	nodes := []agentgraph.Node{{ID: sessionID, Runtime: runtime, Attention: attention, UpdatedAt: at}}
	for i, child := range children {
		child.ParentID = sessionID
		if child.ID == "" {
			child.ID = sessionID + "-child-" + string(rune('a'+i))
		}
		nodes = append(nodes, child)
	}
	return agentgraph.Observation{Provider: provider, RootID: sessionID, Source: source,
		ObservedAt: at, FreshUntil: at.Add(lease), Nodes: nodes}
}

var workingChild = agentgraph.Node{Runtime: agentgraph.RuntimeActive}

func TestGraphBuildersShouldDeclareKindAndOrderWhenBuiltFromTheirSource(t *testing.T) {
	o := func(p agentgraph.ProviderKind, s agentgraph.SourceKind) agentgraph.Observation {
		return observation(p, s, t0, time.Minute, agentgraph.RuntimeActive, agentgraph.AttentionNone)
	}
	for _, tc := range []struct {
		name      string
		c         Candidate
		kind      statusexplain.EvidenceKind
		source    agentgraph.SourceKind
		complete  bool
		eventTime bool
	}{
		{"claude graph", ClaudeGraph(lifetime, o(agentgraph.ProviderClaude, agentgraph.SourceClaudeTranscript)),
			statusexplain.EvidenceProviderSnapshot, agentgraph.SourceClaudeTranscript, false, false},
		{"claude hook", ClaudeHook(lifetime, o(agentgraph.ProviderClaude, agentgraph.SourceHook)),
			statusexplain.EvidenceHook, agentgraph.SourceHook, false, false},
		{"codex app-server", CodexAppServer(lifetime, o(agentgraph.ProviderCodex, agentgraph.SourceCodexAppServer)),
			statusexplain.EvidenceProviderSnapshot, agentgraph.SourceCodexAppServer, false, true},
		{"codex hook", CodexHook(lifetime, o(agentgraph.ProviderCodex, agentgraph.SourceHook)),
			statusexplain.EvidenceHook, agentgraph.SourceHook, false, true},
		{"codex child hooks", CodexChildHooks(lifetime, o(agentgraph.ProviderCodex, agentgraph.SourceHook)),
			statusexplain.EvidenceHookEdge, agentgraph.SourceHook, false, false},
		// The transcript-poll correction borrowed the graph's source; it now
		// names its own, whatever the observation said.
		{"codex rollout tail", CodexRolloutTail(lifetime, o(agentgraph.ProviderCodex, agentgraph.SourceCodexAppServer)),
			statusexplain.EvidenceTranscript, agentgraph.SourceCodexRollout, false, true},
		{"pi hook", PiHook(lifetime, o(agentgraph.ProviderPi, agentgraph.SourceHook)),
			statusexplain.EvidenceHook, agentgraph.SourceHook, true, false},
		{"pi session tail", PiSessionTail(lifetime, o(agentgraph.ProviderPi, agentgraph.SourceHook)),
			statusexplain.EvidenceTranscript, agentgraph.SourcePiSessionFile, false, false},
		{"restored", RestoredLastKnown(lifetime, o(agentgraph.ProviderPi, agentgraph.SourceHook)),
			statusexplain.EvidenceRestored, agentgraph.SourceRestoredLastKnown, false, false},
	} {
		c := tc.c
		if c.Kind != tc.kind || c.Source != tc.source || c.CompleteLifecycle != tc.complete || c.EventTimeOrder != tc.eventTime {
			t.Errorf("%s: kind %s source %s complete %v event-time %v, want %s %s %v %v",
				tc.name, c.Kind, c.Source, c.CompleteLifecycle, c.EventTimeOrder, tc.kind, tc.source, tc.complete, tc.eventTime)
		}
		if c.Identity.SessionID != sessionID || !c.Identity.StartedAt.Equal(lifetime) || c.Identity.PaneID != "" {
			t.Errorf("%s: identity %+v", tc.name, c.Identity)
		}
		if !c.ObservedAt.Equal(t0) || !c.FreshUntil.Equal(t0.Add(time.Minute)) {
			t.Errorf("%s: window %s..%s, want the observation's", tc.name, c.ObservedAt, c.FreshUntil)
		}
	}
}

func TestGraphBuildersShouldShapeWhatTheirKindMayEstablishWhenTheGraphReportsMore(t *testing.T) {
	red := func(source agentgraph.SourceKind) agentgraph.Observation {
		return observation(agentgraph.ProviderCodex, source, t0, time.Minute, agentgraph.RuntimeActive,
			agentgraph.AttentionUserInput, workingChild)
	}
	for _, tc := range []struct {
		name        string
		c           Candidate
		status      string
		attention   agentgraph.AttentionState
		descendants int
	}{
		{"snapshot keeps the full graph", CodexAppServer(lifetime, red(agentgraph.SourceCodexAppServer)),
			agentgraph.LegacyPermission, agentgraph.AttentionUserInput, 1},
		{"event keeps attention and the children it was composed with", CodexHook(lifetime, red(agentgraph.SourceHook)),
			agentgraph.LegacyPermission, agentgraph.AttentionUserInput, 1},
		{"rollout tail keeps the root's runtime and the children of the graph it corrected", CodexRolloutTail(lifetime, red(agentgraph.SourceCodexRollout)),
			agentgraph.LegacyWorking, agentgraph.AttentionNone, 1},
		{"pi session tail keeps only the root's runtime", PiSessionTail(lifetime, red(agentgraph.SourcePiSessionFile)),
			agentgraph.LegacyWorking, agentgraph.AttentionNone, 0},
		{"partial edge keeps only descendants", CodexChildHooks(lifetime, red(agentgraph.SourceHook)),
			"", agentgraph.AttentionNone, 1},
		{"restored keeps presentation, no attention or descendants", RestoredLastKnown(lifetime, red(agentgraph.SourceHook)),
			agentgraph.LegacyPermission, agentgraph.AttentionNone, 0},
	} {
		if tc.c.Status != tc.status || tc.c.Attention != tc.attention || tc.c.WorkingDescendants != tc.descendants {
			t.Errorf("%s: status %q attention %s descendants %d, want %q %s %d", tc.name,
				tc.c.Status, tc.c.Attention, tc.c.WorkingDescendants, tc.status, tc.attention, tc.descendants)
		}
	}
}

func TestGraphBuildersShouldReportWhatTheEvidenceAssertedWhenItHasAlreadyExpired(t *testing.T) {
	o := observation(agentgraph.ProviderClaude, agentgraph.SourceClaudeTranscript, t0, time.Second, agentgraph.RuntimeActive, agentgraph.AttentionNone)
	c := ClaudeGraph(lifetime, o)
	if c.Status != agentgraph.LegacyWorking {
		t.Fatalf("status %q, want working: a builder reduces at the observation's own time", c.Status)
	}
	if c.Fresh(t0.Add(time.Second)) {
		t.Fatal("a candidate stayed fresh at its deadline")
	}
}

func TestRestoredLastKnownShouldKeepThePersistedDeadlineWhenLoadedLater(t *testing.T) {
	persisted := observation(agentgraph.ProviderPi, agentgraph.SourceHook, t0, 5*time.Minute, agentgraph.RuntimeActive, agentgraph.AttentionNone)
	c := RestoredLastKnown(lifetime, persisted)
	if !c.ObservedAt.Equal(t0) || !c.FreshUntil.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("restored window %s..%s, want the persisted one", c.ObservedAt, c.FreshUntil)
	}
	if c.Fresh(t0.Add(5 * time.Minute)) {
		t.Fatal("restored evidence outlived its persisted deadline")
	}
}

func TestHerdrShouldMapEveryRawStatusWhenRead(t *testing.T) {
	for raw, want := range map[string]string{
		"working": agentgraph.LegacyWorking, "blocked": agentgraph.LegacyPermission,
		"idle": agentgraph.LegacyIdle, "done": agentgraph.LegacyIdle, "unknown": "", "": "", "bogus": "",
	} {
		c := Herdr(HerdrReading{PaneID: "w1:p1", Agent: "claude", Status: raw, Live: true, Since: t0}, t0)
		if c.Status != want || c.Attention != agentgraph.AttentionNone {
			t.Errorf("herdr %q mapped to %q attention %s, want %q none", raw, c.Status, c.Attention, want)
		}
		if c.Kind != statusexplain.EvidenceTerminal || c.Source != agentgraph.SourceHerdr {
			t.Errorf("herdr %q: kind %s source %s", raw, c.Kind, c.Source)
		}
	}
}

func TestHerdrShouldHoldWithoutDeadlineWhileFollowedAndExpireWhenWithdrawn(t *testing.T) {
	live := Herdr(HerdrReading{PaneID: "w1:p1", Agent: "claude", Status: "working", Live: true, Since: t0}, t0)
	if !live.FreshUntil.IsZero() || !live.Fresh(t0.Add(30*24*time.Hour)) {
		t.Fatalf("a followed reading has deadline %s", live.FreshUntil)
	}
	withdrawn := Herdr(HerdrReading{PaneID: "w1:p1", Agent: "claude", Status: "working", Live: false, Since: t0}, t0.Add(time.Second))
	if withdrawn.Fresh(t0.Add(time.Second)) {
		t.Fatal("a withdrawn reading was fresh")
	}
	if withdrawn.Identity != (Identity{Provider: "claude", PaneID: "w1:p1"}) {
		t.Fatalf("identity %+v", withdrawn.Identity)
	}
}

func TestHerdrShouldDateAReadingNowWhenItsStartIsUnsetOrInTheFuture(t *testing.T) {
	for _, since := range []time.Time{{}, t0.Add(time.Hour)} {
		c := Herdr(HerdrReading{PaneID: "w1:p1", Agent: "claude", Status: "idle", Live: true, Since: since}, t0)
		if !c.ObservedAt.Equal(t0) {
			t.Errorf("since %s dated %s, want now", since, c.ObservedAt)
		}
	}
}

func TestCandidateFreshShouldFailClosedWhenANonTerminalKindHasNoDeadline(t *testing.T) {
	c := ClaudeGraph(lifetime, observation(agentgraph.ProviderClaude, agentgraph.SourceClaudeTranscript, t0, 0,
		agentgraph.RuntimeActive, agentgraph.AttentionNone))
	c.FreshUntil = time.Time{}
	if c.Fresh(t0) {
		t.Fatal("a snapshot with no deadline was fresh")
	}
	if c.Fresh(t0.Add(-time.Second)) {
		t.Fatal("a candidate was fresh before it was observed")
	}
}
