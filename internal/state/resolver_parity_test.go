package state

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
	"github.com/tjmisko/switchboard/internal/statusresolve"
)

// Parity between today's projection precedence (precedence_characterization_test.go)
// and the pure resolver (#96), which is not wired in yet. Each row runs one
// situation through today's code, live, and through the resolver's builders and
// Resolve. A row may differ only on purpose: it names the #96 criterion that
// changes it. Phase 3B switches projection over on the basis of this table.

// parityGraph is a one-root graph from source at at reducing to status.
func parityGraph(t *testing.T, provider agentgraph.ProviderKind, rootID string, source agentgraph.SourceKind, status string, at time.Time, lease time.Duration) *AgentGraph {
	t.Helper()
	root := agentgraph.Node{ID: rootID, UpdatedAt: at}
	nodes := []agentgraph.Node{}
	switch status {
	case StatusWorking:
		root.Runtime = agentgraph.RuntimeActive
	case StatusIdle:
		root.Runtime = agentgraph.RuntimeIdle
	case StatusPermission:
		root.Runtime, root.Attention = agentgraph.RuntimeActive, agentgraph.AttentionApproval
	case StatusDelegating:
		root.Runtime = agentgraph.RuntimeIdle
		nodes = append(nodes, agentgraph.Node{ID: rootID + "-child", ParentID: rootID, Runtime: agentgraph.RuntimeActive, UpdatedAt: at})
	}
	graph, err := ProjectAgentGraph(agentgraph.Observation{
		Provider: provider, RootID: rootID, Source: source, ObservedAt: at, FreshUntil: at.Add(lease),
		Nodes: append([]agentgraph.Node{root}, nodes...),
	}, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Summary.Status != status {
		t.Fatalf("graph for %q reduced to %q", status, graph.Summary.Status)
	}
	return graph
}

// resolverTarget is the tracked agent a session's candidates must be about.
func resolverTarget(s *Session) statusresolve.Target {
	t := statusresolve.Target{Root: s.explainRoot()}
	if s.Herdr != nil {
		t.PaneID = s.Herdr.PaneID
	}
	return t
}

func herdrCandidate(r HerdrReading, now time.Time) statusresolve.Candidate {
	return statusresolve.Herdr(statusresolve.HerdrReading{PaneID: r.PaneID, Agent: r.Agent, Status: r.Status, Live: r.Live, Since: r.Since}, now)
}

type parityRow struct {
	name      string
	today     string // the published status today's code gives, checked live
	resolver  string // the status the resolver gives
	criterion string // why they differ; "" when they must agree
}

func checkParity(t *testing.T, row parityRow, today, resolved string) {
	t.Helper()
	if today != row.today {
		t.Errorf("%s: today's code published %q, the table says %q", row.name, today, row.today)
	}
	if resolved != row.resolver {
		t.Errorf("%s: resolver decided %q, want %q", row.name, resolved, row.resolver)
	}
	if (row.today != row.resolver) != (row.criterion != "") {
		t.Errorf("%s: today %q, resolver %q: a difference must name its criterion, and only a difference may", row.name, row.today, row.resolver)
	}
}

const (
	criterionAttention = "fresh unresolved provider attention survives terminal working or idle readings"
	criterionIdentity  = "terminal readings must match the tracked agent and terminal association"
)

func TestResolverParityShouldMatchHerdrOverAProviderGraphExceptWhereIssue96ChangesIt(t *testing.T) {
	for _, tc := range []struct {
		parityRow
		agent, herdr string
		herdrAgent   string
		live         bool
		graph        string
	}{
		{parityRow{"herdr working over claude idle", StatusWorking, StatusWorking, ""}, AgentKindClaude, HerdrWorking, "claude", true, StatusIdle},
		{parityRow{"herdr working over claude red", StatusWorking, StatusPermission, criterionAttention}, AgentKindClaude, HerdrWorking, "claude", true, StatusPermission},
		{parityRow{"herdr blocked over claude working", StatusPermission, StatusPermission, ""}, AgentKindClaude, HerdrBlocked, "claude", true, StatusWorking},
		{parityRow{"herdr idle over claude working", StatusIdle, StatusIdle, ""}, AgentKindClaude, HerdrIdle, "claude", true, StatusWorking},
		{parityRow{"herdr done over claude red", StatusIdle, StatusPermission, criterionAttention}, AgentKindClaude, HerdrDone, "claude", true, StatusPermission},
		{parityRow{"herdr idle over claude delegating", StatusDelegating, StatusDelegating, ""}, AgentKindClaude, HerdrIdle, "claude", true, StatusDelegating},
		{parityRow{"herdr done over claude delegating", StatusDelegating, StatusDelegating, ""}, AgentKindClaude, HerdrDone, "claude", true, StatusDelegating},
		{parityRow{"herdr unknown under claude idle", StatusIdle, StatusIdle, ""}, AgentKindClaude, HerdrUnknown, "claude", true, StatusIdle},
		{parityRow{"herdr unfollowed under claude idle", StatusIdle, StatusIdle, ""}, AgentKindClaude, HerdrWorking, "claude", false, StatusIdle},
		{parityRow{"herdr sees codex in a claude pane", StatusWorking, StatusIdle, criterionIdentity}, AgentKindClaude, HerdrWorking, "codex", true, StatusIdle},
		{parityRow{"herdr sees no agent in a claude pane", StatusWorking, StatusIdle, criterionIdentity}, AgentKindClaude, HerdrWorking, "", true, StatusIdle},
		// herdrAuthority reads the wall clock for the Codex rule, so these rows
		// are dated from it; the resolver gets the same instant.
		{parityRow{"herdr working over codex input", StatusPermission, StatusPermission, ""}, AgentKindCodex, HerdrWorking, "codex", true, StatusPermission},
		{parityRow{"herdr working over codex working", StatusWorking, StatusWorking, ""}, AgentKindCodex, HerdrWorking, "codex", true, StatusWorking},
		{parityRow{"herdr idle over codex input", StatusPermission, StatusPermission, ""}, AgentKindCodex, HerdrIdle, "codex", true, StatusPermission},
	} {
		at, provider, source, rootID := herdrT0, agentgraph.ProviderClaude, agentgraph.SourceClaudeTranscript, "sess-1"
		s := &Session{PID: 40, StartedAt: herdrT0.Add(-time.Hour), Agent: tc.agent}
		if tc.agent == AgentKindCodex {
			at, provider, source, rootID = time.Now(), agentgraph.ProviderCodex, agentgraph.SourceCodexAppServer, "thread-1"
			s.Codex = &AgentInfo{}
		} else {
			s.Claude = &AgentInfo{}
		}
		graph := parityGraph(t, provider, rootID, source, tc.graph, at, time.Hour)
		r := reading(tc.herdr, true, at)
		r.Agent = tc.herdrAgent
		s.SetAgentGraph(graph, at)
		s.SetHerdr(r, at)
		if !tc.live {
			r.Live = false
			s.SetHerdr(r, at)
		}
		today := s.Enrichment().Status

		build := statusresolve.ClaudeGraph
		if tc.agent == AgentKindCodex {
			build = statusresolve.CodexAppServer
		}
		candidates := []statusresolve.Candidate{build(s.StartedAt, graph.observation(provider)), herdrCandidate(r, at)}
		d := statusresolve.Resolve(resolverTarget(s), candidates, statusexplain.Decision{}, at)
		checkParity(t, tc.parityRow, today, d.Status)
	}
}

func TestResolverParityShouldMatchPiPrecedenceWhenEachSourceIsAvailable(t *testing.T) {
	at := herdrT0.Add(time.Second)
	later := at.Add(time.Millisecond)
	for _, tc := range []struct {
		parityRow
		herdr string // "" for no herdr reading
		graph agentgraph.SourceKind
		lease time.Duration
		red   bool
	}{
		{parityRow{"hook over herdr", StatusWorking, StatusWorking, ""}, HerdrIdle, agentgraph.SourceHook, time.Minute, false},
		{parityRow{"herdr over session file", StatusIdle, StatusIdle, ""}, HerdrIdle, agentgraph.SourcePiSessionFile, time.Minute, false},
		{parityRow{"herdr over lapsed hook", StatusIdle, StatusIdle, ""}, HerdrIdle, agentgraph.SourceHook, time.Nanosecond, false},
		{parityRow{"session file alone", StatusWorking, StatusWorking, ""}, "", agentgraph.SourcePiSessionFile, time.Minute, false},
		{parityRow{"lapsed hook alone", "", "", ""}, "", agentgraph.SourceHook, time.Nanosecond, false},
		{parityRow{"herdr unknown alone", "", "", ""}, HerdrUnknown, "", 0, false},
		{parityRow{"hook dialog under herdr working", StatusPermission, StatusPermission, ""}, HerdrWorking, agentgraph.SourceHook, time.Minute, true},
		{parityRow{"hook dialog under herdr blocked", StatusPermission, StatusPermission, ""}, HerdrBlocked, agentgraph.SourceHook, time.Minute, true},
	} {
		s := &Session{PID: 43, StartedAt: herdrT0.Add(-time.Hour), Agent: AgentKindPi, Pi: &AgentInfo{SessionID: piSessionID}}
		var candidates []statusresolve.Candidate
		if tc.herdr != "" {
			r := piReading(tc.herdr, herdrT0)
			s.SetHerdr(r, herdrT0)
			candidates = append(candidates, herdrCandidate(r, later))
		}
		if tc.graph != "" {
			attention := agentgraph.AttentionNone
			if tc.red {
				attention = agentgraph.AttentionUserInput
			}
			graph := piHookGraph(t, agentgraph.RuntimeActive, attention, at, tc.lease)
			graph.Source = tc.graph
			s.SetPiHookGraph(graph, at)
			build := statusresolve.PiHook
			if tc.graph == agentgraph.SourcePiSessionFile {
				build = statusresolve.PiSessionTail
			}
			candidates = append(candidates, build(s.StartedAt, graph.observation(agentgraph.ProviderPi)))
		}
		s.ReprojectPi(later)
		d := statusresolve.Resolve(resolverTarget(s), candidates, statusexplain.Decision{}, later)
		checkParity(t, tc.parityRow, s.Pi.Status, d.Status)
	}
}

func TestResolverParityShouldMatchHerdrOnlyAgentsWhenFollowedAndWhenNot(t *testing.T) {
	s := &Session{PID: 45, StartedAt: herdrT0.Add(-time.Hour), Agent: "gemini"}
	r := reading(HerdrBlocked, true, herdrT0)
	r.Agent = "gemini"
	s.SetHerdr(r, herdrT0)
	followed := statusresolve.Resolve(resolverTarget(s), []statusresolve.Candidate{herdrCandidate(r, herdrT0)}, statusexplain.Decision{}, herdrT0)
	checkParity(t, parityRow{"herdr-only blocked", StatusPermission, StatusPermission, ""}, s.publishedStatus(herdrT0), followed.Status)

	later := herdrT0.Add(time.Second)
	r.Live, r.Since = false, later
	s.SetHerdr(r, later)
	dropped := statusresolve.Resolve(resolverTarget(s), []statusresolve.Candidate{herdrCandidate(r, later)}, followed, later)
	checkParity(t, parityRow{"herdr-only unfollowed", "", "", ""}, s.publishedStatus(later), dropped.Status)
}
