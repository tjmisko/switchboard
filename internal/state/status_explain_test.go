package state

import (
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// codexGraph is a fresh Codex app-server graph at at whose root carries
// attention, projected as the coordinator projects one.
func codexGraph(t *testing.T, runtime agentgraph.RuntimeState, attention agentgraph.AttentionState, at time.Time, lease time.Duration) *AgentGraph {
	t.Helper()
	graph, err := ProjectAgentGraph(agentgraph.Observation{
		Provider: agentgraph.ProviderCodex, RootID: "thread-1", Source: agentgraph.SourceCodexAppServer,
		ObservedAt: at, FreshUntil: at.Add(lease), Complete: true,
		Nodes: []agentgraph.Node{{ID: "thread-1", Runtime: runtime, Attention: attention, UpdatedAt: at}},
	}, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func claudeGraph(t *testing.T, runtime agentgraph.RuntimeState, at time.Time, lease time.Duration) *AgentGraph {
	t.Helper()
	graph, err := ProjectAgentGraph(agentgraph.Observation{
		Provider: agentgraph.ProviderClaude, RootID: "sess-1", Source: agentgraph.SourceClaudeTranscript,
		ObservedAt: at, FreshUntil: at.Add(lease),
		Nodes: []agentgraph.Node{{ID: "sess-1", Runtime: runtime, UpdatedAt: at}},
	}, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func onlyRejected(t *testing.T, d statusexplain.Decision) statusexplain.Candidate {
	t.Helper()
	if len(d.Rejected) != 1 {
		t.Fatalf("rejected = %+v, want exactly one candidate", d.Rejected)
	}
	return d.Rejected[0]
}

// Acceptance criterion 1, Codex: an open input request held while herdr reads
// the screen as working.
func TestExplainStatusShouldExplainPermissionAndTheRejectedTerminalReadingWhenACodexInputRequestIsUnresolved(t *testing.T) {
	now := herdrT0.Add(time.Minute)
	s := &Session{PID: 30, StartedAt: herdrT0, Agent: AgentKindCodex, Codex: &AgentInfo{}}
	s.SetHerdr(codexReading(HerdrWorking, true, now.Add(-time.Minute)), now)
	s.SetAgentGraph(codexGraph(t, agentgraph.RuntimeActive, agentgraph.AttentionUserInput, now, time.Hour), now)

	d := s.ExplainStatus(now)
	if d.Status != StatusPermission || d.Reason != statusexplain.ReasonAttentionHeld {
		t.Fatalf("decision = %+v, want permission for attention_held", d.Choice)
	}
	if d.Source != string(agentgraph.SourceCodexAppServer) || d.EvidenceKind != statusexplain.EvidenceProviderSnapshot {
		t.Fatalf("selected source = %q/%q, want the app-server graph", d.Source, d.EvidenceKind)
	}
	rejected := onlyRejected(t, d)
	if rejected.Source != string(agentgraph.SourceHerdr) || rejected.Status != StatusWorking ||
		rejected.RejectReason != statusexplain.ReasonAttentionHeld {
		t.Fatalf("rejected = %+v, want herdr's working reading rejected for attention_held", rejected)
	}
}

// Acceptance criterion 1, Pi: an open dialog held by the hook while herdr
// reads the screen as working.
func TestExplainStatusShouldExplainPermissionAndTheRejectedTerminalReadingWhenAPiDialogIsOpen(t *testing.T) {
	s := boundPi(t, HerdrIdle)
	at := herdrT0.Add(time.Second)
	s.SetPiHookGraph(piHookGraph(t, agentgraph.RuntimeActive, agentgraph.AttentionUserInput, at, time.Minute), at)
	later := at.Add(time.Second)
	s.SetHerdr(piReading(HerdrWorking, later), later)

	d := s.ExplainStatus(later)
	if d.Status != StatusPermission || d.Reason != statusexplain.ReasonEventAuthority || d.Source != string(agentgraph.SourceHook) {
		t.Fatalf("decision = %+v, want permission from the Pi hook", d.Choice)
	}
	if !d.DecidedAt.Equal(later) || !d.FreshUntil.Equal(at.Add(time.Minute)) {
		t.Fatalf("decided_at = %v fresh_until = %v", d.DecidedAt, d.FreshUntil)
	}
	rejected := onlyRejected(t, d)
	if rejected.Source != string(agentgraph.SourceHerdr) || rejected.Status != StatusWorking ||
		rejected.RejectReason != statusexplain.ReasonSourceOutranked {
		t.Fatalf("rejected = %+v, want herdr's working reading outranked by the Pi hook", rejected)
	}
}

// Acceptance criterion 2.
func TestExplainStatusShouldGiveDistinctReasonsWhenBindingIsMissingObservationExpiredOrCoverageUnsupported(t *testing.T) {
	at := herdrT0
	unbound := Session{PID: 1, StartedAt: at, Agent: AgentKindClaude}

	expired := &Session{PID: 2, StartedAt: at, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	expired.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeActive, at, time.Minute), at)
	later := at.Add(2 * time.Minute)
	stale, err := ProjectAgentGraph(agentgraph.Observation{
		Provider: agentgraph.ProviderClaude, RootID: "sess-1", Source: agentgraph.SourceClaudeTranscript,
		ObservedAt: at, FreshUntil: at.Add(time.Minute),
		Nodes: []agentgraph.Node{{ID: "sess-1", Runtime: agentgraph.RuntimeActive, UpdatedAt: at}},
	}, expired.AgentGraph, later)
	if err != nil {
		t.Fatal(err)
	}
	expired.SetAgentGraph(stale, later) // what expireCurrent lands

	unsupported := Session{PID: 3, StartedAt: at, Agent: "gemini"}

	got := map[statusexplain.Reason]int{}
	for _, tc := range []struct {
		sess Session
		want statusexplain.Reason
	}{
		{unbound, statusexplain.ReasonBindingMissing},
		{*expired, statusexplain.ReasonObservationExpired},
		{unsupported, statusexplain.ReasonCoverageUnsupported},
	} {
		d := tc.sess.ExplainStatus(later)
		if d.Reason != tc.want || d.Status != "" {
			t.Fatalf("pid %d: decision = %+v, want unknown for %s", tc.sess.PID, d.Choice, tc.want)
		}
		got[d.Reason] = tc.sess.PID
	}
	if len(got) != 3 {
		t.Fatalf("reasons collapsed: %v", got)
	}
	// The expired evidence is the rejected candidate that explains the unknown.
	d := expired.ExplainStatus(later)
	if r := onlyRejected(t, d); !r.FreshUntil.Equal(at.Add(time.Minute)) || r.Source != string(agentgraph.SourceClaudeTranscript) ||
		r.RejectReason != statusexplain.ReasonObservationExpired {
		t.Fatalf("expired decision lost its evidence: %+v", d)
	}
}

func TestExplainStatusShouldReportCoverageUnsupportedWhenALiveHerdrReadingClassifiesNothing(t *testing.T) {
	s := &Session{PID: 4, StartedAt: herdrT0, Agent: "gemini"}
	r := reading(HerdrUnknown, true, herdrT0)
	r.Agent = "gemini"
	s.SetHerdr(r, herdrT0)
	d := s.ExplainStatus(herdrT0)
	if d.Status != "" || d.Reason != statusexplain.ReasonCoverageUnsupported {
		t.Fatalf("decision = %+v, want unknown for coverage_unsupported", d.Choice)
	}
	if c := onlyRejected(t, d); c.Source != string(agentgraph.SourceHerdr) || c.RejectReason != statusexplain.ReasonCoverageUnsupported {
		t.Fatalf("rejected = %+v, want herdr's reading as the evidence that classifies nothing", c)
	}
	r.Status, r.Since = HerdrWorking, herdrT0.Add(time.Second)
	s.SetHerdr(r, herdrT0.Add(time.Second))
	if d := s.ExplainStatus(herdrT0.Add(time.Second)); d.Status != StatusWorking || d.Reason != statusexplain.ReasonTerminalAuthority {
		t.Fatalf("decision = %+v, want working for terminal_authority", d.Choice)
	}
}

func TestExplainStatusShouldReportObservationPendingWhenABoundSessionHasNoGraphYet(t *testing.T) {
	s := Session{PID: 5, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{SessionID: "sess-1"}}
	if d := s.ExplainStatus(herdrT0); d.Reason != statusexplain.ReasonObservationPending {
		t.Fatalf("decision = %+v, want observation_pending", d.Choice)
	}
}

// Acceptance criterion 3, at the session: a record kept on a session whose
// process lifetime changed is not current.
func TestExplainStatusShouldNotExposeThePreviousLifetimesDecisionWhenTheProcessIsReplaced(t *testing.T) {
	s := &Session{PID: 6, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	s.SetHerdr(reading(HerdrWorking, true, herdrT0), herdrT0)
	s.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeIdle, herdrT0, time.Hour), herdrT0)
	if d := s.ExplainStatus(herdrT0); d.Reason != statusexplain.ReasonTerminalAuthority {
		t.Fatalf("first lifetime = %+v, want terminal_authority", d.Choice)
	}

	// The same PID, a new process: the record and the provider state are left
	// over on the struct, the lifetime is not.
	replaced := *s
	replaced.StartedAt = herdrT0.Add(time.Hour)
	replaced.Claude = &AgentInfo{Status: s.Claude.Status}
	replaced.AgentGraph = nil
	replaced.Herdr = nil
	d := replaced.ExplainStatus(herdrT0.Add(time.Hour))
	if d.Reason == statusexplain.ReasonTerminalAuthority || len(d.Rejected) != 0 {
		t.Fatalf("replaced process explained by the previous lifetime: %+v", d)
	}
	if !d.Root.StartedAt.Equal(herdrT0.Add(time.Hour)) {
		t.Fatalf("root = %+v, want the new lifetime", d.Root)
	}
}

func TestExplainStatusShouldRefreshOnlyDecidedAtWhenTheDecisionIsUnchanged(t *testing.T) {
	s := &Session{PID: 7, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	graph := claudeGraph(t, agentgraph.RuntimeActive, herdrT0, time.Minute)
	s.SetAgentGraph(graph, herdrT0)
	first := s.ExplainStatus(herdrT0)
	later := herdrT0.Add(10 * time.Second)
	s.SetAgentGraph(graph, later)
	second := s.ExplainStatus(later)

	if second.Status != StatusWorking || second.Reason != statusexplain.ReasonGraphAuthority ||
		second.Source != string(agentgraph.SourceClaudeTranscript) {
		t.Fatalf("decision = %+v, want working from the transcript graph", second.Choice)
	}
	if !second.FreshUntil.Equal(herdrT0.Add(time.Minute)) || !second.ObservedAt.Equal(herdrT0) {
		t.Fatalf("decision lost its deadline: %+v", second.Choice)
	}
	if !second.DecidedAt.Equal(later) {
		t.Fatalf("decided_at = %v, want %v", second.DecidedAt, later)
	}
	first.DecidedAt, second.DecidedAt = time.Time{}, time.Time{}
	if first.Choice != second.Choice {
		t.Fatalf("unchanged decision changed beyond decided_at:\n%+v\n%+v", first.Choice, second.Choice)
	}
}

func TestExplainStatusShouldExplainHerdrOverridingAGraphWhenHerdrIsLive(t *testing.T) {
	s := &Session{PID: 8, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	s.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeIdle, herdrT0, time.Hour), herdrT0)
	at := herdrT0.Add(time.Second)
	s.SetHerdr(reading(HerdrWorking, true, at), at)

	d := s.ExplainStatus(at)
	if d.Status != StatusWorking || d.Reason != statusexplain.ReasonTerminalAuthority ||
		d.EvidenceKind != statusexplain.EvidenceTerminal || !d.ObservedAt.Equal(at) || !d.FreshUntil.IsZero() {
		t.Fatalf("decision = %+v, want herdr's working as terminal_authority", d.Choice)
	}
	rejected := onlyRejected(t, d)
	if rejected.Status != StatusIdle || rejected.Source != string(agentgraph.SourceClaudeTranscript) ||
		rejected.RejectReason != statusexplain.ReasonSourceOutranked {
		t.Fatalf("rejected = %+v, want the graph's idle outranked by herdr", rejected)
	}
}

func TestExplainStatusShouldExplainHerdrFallingBackWhenThePiHookLeaseLapses(t *testing.T) {
	s := boundPi(t, HerdrIdle)
	at := herdrT0.Add(time.Second)
	s.SetPiHookGraph(piHookGraph(t, agentgraph.RuntimeActive, agentgraph.AttentionNone, at, time.Minute), at)
	lapsed := at.Add(time.Minute)
	s.ReprojectPi(lapsed)

	d := s.ExplainStatus(lapsed)
	if d.Status != StatusIdle || d.Reason != statusexplain.ReasonTerminalAuthority {
		t.Fatalf("decision = %+v, want herdr's idle as terminal_authority", d.Choice)
	}
	rejected := onlyRejected(t, d)
	if rejected.Source != string(agentgraph.SourceHook) || rejected.RejectReason != statusexplain.ReasonObservationExpired {
		t.Fatalf("rejected = %+v, want the lapsed hook graph rejected as observation_expired", rejected)
	}
}

func TestExplainStatusShouldReportLimitedWithTheUnderlyingDecisionBeneathItWhenAUsageLimitIsActive(t *testing.T) {
	s := &Session{PID: 9, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	s.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeIdle, herdrT0, time.Hour), herdrT0)
	resets := herdrT0.Add(3 * time.Hour)
	s.RecordUsageLimit(UsageLimit{ObservedAt: herdrT0, ResetsAt: &resets, Source: UsageLimitSourceClaudeHook})

	at := herdrT0.Add(time.Minute)
	d := s.ExplainStatus(at)
	if d.Status != StatusLimited || d.Reason != statusexplain.ReasonUsageLimitOverlay ||
		d.EvidenceKind != statusexplain.EvidenceUsageLimit || d.Source != UsageLimitSourceClaudeHook ||
		!d.FreshUntil.Equal(resets) || !d.DecidedAt.Equal(at) {
		t.Fatalf("decision = %+v, want limited as usage_limit_overlay until the reset", d.Choice)
	}
	if d.Underlying == nil || d.Underlying.Status != StatusIdle || d.Underlying.Reason != statusexplain.ReasonGraphAuthority {
		t.Fatalf("underlying = %+v, want the graph's idle", d.Underlying)
	}
	// The overlay is publication's: it matches what ProjectPublished shows.
	// ProjectPublished mutates the snapshot it is given, so hand it a detached copy.
	cp := *s
	claude := *s.Claude
	cp.Claude = &claude
	cp.UsageLimit = cloneUsageLimit(s.UsageLimit)
	published := ProjectPublished(Snapshot{Sessions: []Session{cp}}, at)
	if got := published.Sessions[0].Claude.Status; got != d.Status {
		t.Fatalf("published %q, explained %q", got, d.Status)
	}
	if d := s.ExplainStatus(resets); d.Status == StatusLimited || d.Underlying != nil {
		t.Fatalf("lapsed limit still explained: %+v", d)
	}
}

func TestExplainStatusShouldNotOverlayLimitedWhenTheStatusIsPermission(t *testing.T) {
	s := &Session{PID: 11, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	s.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeIdle, herdrT0, time.Hour), herdrT0)
	s.SetHerdr(reading(HerdrBlocked, true, herdrT0), herdrT0)
	s.RecordUsageLimit(UsageLimit{ObservedAt: herdrT0, Source: UsageLimitSourceClaudeHook})
	d := s.ExplainStatus(herdrT0.Add(time.Minute))
	if d.Status != StatusPermission || d.Underlying != nil || d.Reason != statusexplain.ReasonTerminalAuthority {
		t.Fatalf("decision = %+v underlying = %+v, want herdr's permission and no overlay", d.Choice, d.Underlying)
	}
}

func TestExplainStatusShouldSayUnrecordedWhenAPathThatRecordsNothingMovedTheStatus(t *testing.T) {
	s := &Session{PID: 12, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	s.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeIdle, herdrT0, time.Hour), herdrT0)
	s.Claude.Status = StatusWorking // a legacy hook transition
	d := s.ExplainStatus(herdrT0)
	if d.Status != StatusWorking || d.Reason != statusexplain.ReasonUnrecorded || len(d.Rejected) != 0 {
		t.Fatalf("decision = %+v, want working as unrecorded", d)
	}
}

func TestExplainStatusShouldCarryOnlyIdsEnumsAndTimesWhenTheSessionHoldsUserContent(t *testing.T) {
	s := &Session{PID: 13, StartedAt: herdrT0, Agent: AgentKindClaude, CWD: "/home/u/secret-project",
		Claude:      &AgentInfo{Transcript: "/home/u/.claude/projects/x/secret.jsonl", PendingTool: "Bash"},
		DisplayName: &DisplayName{Value: "my secret plan", Origin: DisplayNameNative}}
	s.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeActive, herdrT0, time.Hour), herdrT0)
	text := s.ExplainStatus(herdrT0).Sanitize().Text()
	for _, leaked := range []string{"secret", "Bash", "/home/u"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("explanation carries %q:\n%s", leaked, text)
		}
	}
}

func TestStoreSnapshotShouldCarryTheDecisionRecordWhenASessionIsCopied(t *testing.T) {
	store := New("")
	store.Apply(func(m map[int]*Session) {
		s := &Session{PID: 14, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
		s.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeActive, herdrT0, time.Hour), herdrT0)
		m[14] = s
	})
	snap := store.Snapshot()
	if got := snap.Sessions[0].StatusDecision().Reason; got != statusexplain.ReasonGraphAuthority {
		t.Fatalf("snapshot decision reason = %q", got)
	}
}
