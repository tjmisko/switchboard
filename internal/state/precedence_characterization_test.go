package state

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// Characterization of projection precedence (#95, work unit 6), now decided by
// the one status resolver (#96). Each test is named for the rule it pins and
// asserts both the published status and the reason explain gives for it.
//
// Rows the resolver changed on purpose say so with "#96:" and the acceptance
// criterion that changes them. Reason codes are the resolver's: herdr_override,
// herdr_fallback and herdr_only became terminal_authority; pi_hook_authority
// became event_authority; herdr_yield_attention became attention_held; a Pi
// session file deciding is transcript_authority; delegation kept over herdr's
// idle is descendants_live.

// Rule terminal authority: a live herdr reading of the tracked agent decides a
// Claude session's status over its provider graph, except an open request for
// the user, which it cannot resolve, and background work it cannot see.
func TestPrecedenceHerdrOverrideShouldPublishHerdrsReadingWhenItIsLiveOverAClaudeGraph(t *testing.T) {
	for _, tc := range []struct {
		herdr, agent string
		graph        string
		want         string
		wantReason   statusexplain.Reason
	}{
		{HerdrWorking, "claude", StatusIdle, StatusWorking, statusexplain.ReasonTerminalAuthority},
		// #96: fresh unresolved provider attention survives terminal working
		// or idle readings (was herdr's working, herdr_override).
		{HerdrWorking, "claude", StatusPermission, StatusPermission, statusexplain.ReasonAttentionHeld},
		{HerdrBlocked, "claude", StatusWorking, StatusPermission, statusexplain.ReasonTerminalAuthority},
		{HerdrIdle, "claude", StatusWorking, StatusIdle, statusexplain.ReasonTerminalAuthority},
		// #96: as above (was herdr's idle, herdr_override).
		{HerdrDone, "claude", StatusPermission, StatusPermission, statusexplain.ReasonAttentionHeld},
		{HerdrIdle, "claude", StatusDelegating, StatusDelegating, statusexplain.ReasonDescendantsLive},
		{HerdrDone, "claude", StatusDelegating, StatusDelegating, statusexplain.ReasonDescendantsLive},
		// #96: terminal readings must match the tracked agent and terminal
		// association (was herdr's working, herdr_override).
		{HerdrWorking, "codex", StatusIdle, StatusIdle, statusexplain.ReasonGraphAuthority},
		{HerdrWorking, "", StatusIdle, StatusIdle, statusexplain.ReasonGraphAuthority},
	} {
		s := graphSession(t, tc.graph, herdrT0)
		r := reading(tc.herdr, true, herdrT0)
		r.Agent = tc.agent
		s.SetHerdr(r, herdrT0)
		if s.Claude.Status != tc.want {
			t.Errorf("herdr %s (%q) over graph %s published %q, want %q", tc.herdr, tc.agent, tc.graph, s.Claude.Status, tc.want)
		}
		if got := s.ExplainStatus(herdrT0).Reason; got != tc.wantReason {
			t.Errorf("herdr %s (%q) over graph %s explained as %q, want %q", tc.herdr, tc.agent, tc.graph, got, tc.wantReason)
		}
	}
}

// Rule herdr decides nothing when unknown or not followed: the graph decides.
func TestPrecedenceHerdrShouldDecideNothingWhenItsReadingIsUnknownOrNotLive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		herdr      string
		live       bool
		wantReason statusexplain.Reason
	}{
		{"unknown", HerdrUnknown, true, statusexplain.ReasonGraphAuthority},
		{"not live", HerdrWorking, false, statusexplain.ReasonGraphAuthority},
	} {
		s := &Session{PID: 40, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
		s.SetAgentGraph(claudeGraph(t, agentgraph.RuntimeIdle, herdrT0, time.Hour), herdrT0)
		if tc.live {
			s.SetHerdr(reading(tc.herdr, true, herdrT0), herdrT0)
		} else {
			// A followed pane that drops: Live=false withdraws authority.
			s.SetHerdr(reading(HerdrWorking, true, herdrT0), herdrT0)
			s.SetHerdr(reading(tc.herdr, false, herdrT0), herdrT0)
		}
		if s.Claude.Status != StatusIdle {
			t.Errorf("%s: published %q, want the graph's idle", tc.name, s.Claude.Status)
		}
		if got := s.ExplainStatus(herdrT0).Reason; got != tc.wantReason {
			t.Errorf("%s: explained as %q, want %q", tc.name, got, tc.wantReason)
		}
	}
}

// Rule attention held: fresh Codex input or approval attention holds against
// any live herdr reading; expired attention does not.
func TestPrecedenceHerdrShouldYieldOnlyToFreshCodexAttentionWhenBothAreLive(t *testing.T) {
	now := herdrT0
	for _, tc := range []struct {
		name       string
		attention  agentgraph.AttentionState
		lease      time.Duration
		want       string
		wantReason statusexplain.Reason
	}{
		{"user input", agentgraph.AttentionUserInput, time.Hour, StatusPermission, statusexplain.ReasonAttentionHeld},
		{"approval", agentgraph.AttentionApproval, time.Hour, StatusPermission, statusexplain.ReasonAttentionHeld},
		{"no attention", agentgraph.AttentionNone, time.Hour, StatusWorking, statusexplain.ReasonTerminalAuthority},
	} {
		s := &Session{PID: 41, StartedAt: statusLifetime, Agent: AgentKindCodex, Codex: &AgentInfo{}}
		s.SetHerdr(codexReading(HerdrWorking, true, now), now)
		s.SetAgentGraph(codexGraph(t, agentgraph.RuntimeActive, tc.attention, now, tc.lease), now)
		if s.Codex.Status != tc.want {
			t.Errorf("%s: published %q, want %q", tc.name, s.Codex.Status, tc.want)
		}
		if got := s.ExplainStatus(now).Reason; got != tc.wantReason {
			t.Errorf("%s: explained as %q, want %q", tc.name, got, tc.wantReason)
		}
	}

	// #96: fresh unresolved provider attention survives terminal working or
	// idle readings, Claude's included (was herdr's working: before the
	// resolver only Codex attention held).
	claude := &Session{PID: 42, StartedAt: statusLifetime, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	claude.SetHerdr(reading(HerdrWorking, true, now), now)
	claude.SetAgentGraph(claudeStatusGraph(t, agentgraph.SourceClaudeTranscript, StatusPermission, now, time.Hour), now)
	if claude.Claude.Status != StatusPermission {
		t.Errorf("claude approval graph under live herdr working published %q, want the request held", claude.Claude.Status)
	}
}

// Rule Pi precedence: fresh hook > live herdr > session file or restored
// graph within its lease > unknown.
func TestPrecedencePiShouldPreferHookThenHerdrThenSessionFileWhenEachIsAvailable(t *testing.T) {
	at := herdrT0.Add(time.Second)
	for _, tc := range []struct {
		name       string
		herdr      string // "" for no live herdr
		graph      agentgraph.SourceKind
		lease      time.Duration
		want       string
		wantReason statusexplain.Reason
	}{
		{"hook over herdr", HerdrIdle, agentgraph.SourceHook, time.Minute, StatusWorking, statusexplain.ReasonEventAuthority},
		{"herdr over session file", HerdrIdle, agentgraph.SourcePiSessionFile, time.Minute, StatusIdle, statusexplain.ReasonTerminalAuthority},
		{"herdr over lapsed hook", HerdrIdle, agentgraph.SourceHook, 0, StatusIdle, statusexplain.ReasonTerminalAuthority},
		{"session file alone", "", agentgraph.SourcePiSessionFile, time.Minute, StatusWorking, statusexplain.ReasonTranscriptAuthority},
		{"lapsed hook alone", "", agentgraph.SourceHook, 0, "", statusexplain.ReasonObservationExpired},
		{"herdr unknown alone", HerdrUnknown, "", 0, "", statusexplain.ReasonCoverageUnsupported},
	} {
		s := &Session{PID: 43, StartedAt: statusLifetime, Agent: AgentKindPi, Pi: &AgentInfo{SessionID: piSessionID}}
		if tc.herdr != "" {
			s.SetHerdr(piReading(tc.herdr, herdrT0), herdrT0)
		}
		if tc.graph != "" {
			lease := tc.lease
			if lease == 0 {
				lease = time.Nanosecond
			}
			graph := piSourceGraph(t, tc.graph, agentgraph.RuntimeActive, at, lease)
			if tc.graph == agentgraph.SourcePiSessionFile {
				s.SetPiSessionFileGraph(graph, at)
			} else {
				s.SetPiHookGraph(graph, at)
			}
		}
		later := at.Add(time.Millisecond)
		s.ReprojectPi(later)
		if s.Pi.Status != tc.want {
			t.Errorf("%s: published %q, want %q", tc.name, s.Pi.Status, tc.want)
		}
		if got := s.ExplainStatus(later).Reason; got != tc.wantReason {
			t.Errorf("%s: explained as %q, want %q", tc.name, got, tc.wantReason)
		}
	}
}

// Rule usage_limit_overlay: publication reads limited over every status but
// permission, until the limit lapses.
func TestPrecedenceUsageLimitShouldOverlayEveryStatusButPermissionWhenActive(t *testing.T) {
	resets := herdrT0.Add(time.Hour)
	for _, tc := range []struct {
		status string
		want   string
	}{
		{StatusWorking, StatusLimited},
		{StatusIdle, StatusLimited},
		{StatusDelegating, StatusLimited},
		{"", StatusLimited},
		{StatusPermission, StatusPermission},
	} {
		s := Session{PID: 44, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{Status: tc.status},
			UsageLimit: &UsageLimit{ObservedAt: herdrT0, ResetsAt: &resets}}
		published := ProjectPublished(Snapshot{Sessions: []Session{s}}, herdrT0.Add(time.Minute))
		if got := published.Sessions[0].Claude.Status; got != tc.want {
			t.Errorf("limit over %q published %q, want %q", tc.status, got, tc.want)
		}
		lapsed := Session{PID: 44, Agent: AgentKindClaude, Claude: &AgentInfo{Status: tc.status},
			UsageLimit: &UsageLimit{ObservedAt: herdrT0, ResetsAt: &resets}}
		after := ProjectPublished(Snapshot{Sessions: []Session{lapsed}}, resets)
		if got := after.Sessions[0].Claude.Status; got != tc.status || after.Sessions[0].UsageLimit != nil {
			t.Errorf("lapsed limit over %q published %q", tc.status, got)
		}
	}
}

// Rule herdr only: an agent with no provider adapter publishes its herdr graph,
// and a reading that is no longer followed reduces to unknown.
func TestPrecedenceHerdrOnlyShouldPublishTheHerdrGraphWhenTheAgentHasNoProvider(t *testing.T) {
	s := &Session{PID: 45, StartedAt: statusLifetime, Agent: "gemini"}
	r := reading(HerdrBlocked, true, herdrT0)
	r.Agent = "gemini"
	s.SetHerdr(r, herdrT0)
	if got := s.publishedStatus(herdrT0); got != StatusPermission {
		t.Fatalf("herdr blocked published %q", got)
	}
	if got := s.ExplainStatus(herdrT0).Reason; got != statusexplain.ReasonTerminalAuthority {
		t.Fatalf("explained as %q", got)
	}
	later := herdrT0.Add(time.Second)
	r.Live, r.Since = false, later
	s.SetHerdr(r, later)
	if got := s.publishedStatus(later); got != "" {
		t.Fatalf("unfollowed herdr published %q, want unknown", got)
	}
	if got := s.ExplainStatus(later).Reason; got != statusexplain.ReasonObservationExpired {
		t.Fatalf("unfollowed herdr explained as %q", got)
	}
}
