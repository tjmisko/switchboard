package state

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// Characterization of today's projection precedence (#95, work unit 6). Each
// test is named for the rule it pins and asserts both the published status and
// the reason explain gives for it. These are #96's regression suite: the pure
// resolver must reproduce every row, or change it on purpose.

// Rule herdr_override: a live herdr reading decides a Claude session's status
// over its provider graph, whatever the graph says, including a Claude red.
func TestPrecedenceHerdrOverrideShouldPublishHerdrsReadingWhenItIsLiveOverAClaudeGraph(t *testing.T) {
	for _, tc := range []struct {
		herdr string
		graph string
		want  string
	}{
		{HerdrWorking, StatusIdle, StatusWorking},
		{HerdrWorking, StatusPermission, StatusWorking},
		{HerdrBlocked, StatusWorking, StatusPermission},
		{HerdrIdle, StatusWorking, StatusIdle},
		{HerdrDone, StatusPermission, StatusIdle},
		{HerdrIdle, StatusDelegating, StatusDelegating},
		{HerdrDone, StatusDelegating, StatusDelegating},
	} {
		s := graphSession(tc.graph, herdrT0)
		s.StartedAt = herdrT0
		s.SetHerdr(reading(tc.herdr, true, herdrT0), herdrT0)
		if s.Claude.Status != tc.want {
			t.Errorf("herdr %s over graph %s published %q, want %q", tc.herdr, tc.graph, s.Claude.Status, tc.want)
		}
		if got := s.ExplainStatus(herdrT0).Reason; got != statusexplain.ReasonHerdrOverride {
			t.Errorf("herdr %s over graph %s explained as %q", tc.herdr, tc.graph, got)
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

// Rule herdr_yield_attention: fresh Codex input or approval attention holds
// against any live herdr reading; expired attention does not, and Claude's
// attention never does.
func TestPrecedenceHerdrShouldYieldOnlyToFreshCodexAttentionWhenBothAreLive(t *testing.T) {
	now := time.Now() // herdrAuthority reads the wall clock for this rule
	for _, tc := range []struct {
		name       string
		attention  agentgraph.AttentionState
		lease      time.Duration
		want       string
		wantReason statusexplain.Reason
	}{
		{"user input", agentgraph.AttentionUserInput, time.Hour, StatusPermission, statusexplain.ReasonHerdrYieldAttention},
		{"approval", agentgraph.AttentionApproval, time.Hour, StatusPermission, statusexplain.ReasonHerdrYieldAttention},
		{"no attention", agentgraph.AttentionNone, time.Hour, StatusWorking, statusexplain.ReasonHerdrOverride},
	} {
		s := &Session{PID: 41, StartedAt: herdrT0, Agent: AgentKindCodex, Codex: &AgentInfo{}}
		s.SetHerdr(reading(HerdrWorking, true, now), now)
		s.SetAgentGraph(codexGraph(t, agentgraph.RuntimeActive, tc.attention, now, tc.lease), now)
		if s.Codex.Status != tc.want {
			t.Errorf("%s: published %q, want %q", tc.name, s.Codex.Status, tc.want)
		}
		if got := s.ExplainStatus(now).Reason; got != tc.wantReason {
			t.Errorf("%s: explained as %q, want %q", tc.name, got, tc.wantReason)
		}
	}

	claude := &Session{PID: 42, StartedAt: herdrT0, Agent: AgentKindClaude, Claude: &AgentInfo{}}
	claude.SetHerdr(reading(HerdrWorking, true, now), now)
	claude.AgentGraph = &AgentGraph{RootID: "sess-1", ObservedAt: now, FreshUntil: now.Add(time.Hour),
		Summary: AgentGraphSummary{Status: StatusPermission, Attention: agentgraph.AttentionApproval, Since: now}}
	claude.SetAgentGraph(claude.AgentGraph, now)
	if claude.Claude.Status != StatusWorking {
		t.Errorf("claude approval graph under live herdr working published %q, want herdr's working", claude.Claude.Status)
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
		{"hook over herdr", HerdrIdle, agentgraph.SourceHook, time.Minute, StatusWorking, statusexplain.ReasonPiHookAuthority},
		{"herdr over session file", HerdrIdle, agentgraph.SourcePiSessionFile, time.Minute, StatusIdle, statusexplain.ReasonHerdrFallback},
		{"herdr over lapsed hook", HerdrIdle, agentgraph.SourceHook, 0, StatusIdle, statusexplain.ReasonHerdrFallback},
		{"session file alone", "", agentgraph.SourcePiSessionFile, time.Minute, StatusWorking, statusexplain.ReasonGraphAuthority},
		{"lapsed hook alone", "", agentgraph.SourceHook, 0, "", statusexplain.ReasonObservationExpired},
		{"herdr unknown alone", HerdrUnknown, "", 0, "", statusexplain.ReasonCoverageUnsupported},
	} {
		s := &Session{PID: 43, StartedAt: herdrT0, Agent: AgentKindPi, Pi: &AgentInfo{SessionID: piSessionID}}
		if tc.herdr != "" {
			s.SetHerdr(piReading(tc.herdr, herdrT0), herdrT0)
		}
		if tc.graph != "" {
			lease := tc.lease
			if lease == 0 {
				lease = time.Nanosecond
			}
			s.SetPiHookGraph(piSourceGraph(t, tc.graph, agentgraph.RuntimeActive, at, lease), at)
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

// Rule herdr_only: an agent with no provider adapter publishes its herdr graph,
// and a reading that is no longer followed reduces to unknown.
func TestPrecedenceHerdrOnlyShouldPublishTheHerdrGraphWhenTheAgentHasNoProvider(t *testing.T) {
	s := &Session{PID: 45, StartedAt: herdrT0, Agent: "gemini"}
	s.SetHerdr(reading(HerdrBlocked, true, herdrT0), herdrT0)
	if got := s.publishedStatus(herdrT0); got != StatusPermission {
		t.Fatalf("herdr blocked published %q", got)
	}
	if got := s.ExplainStatus(herdrT0).Reason; got != statusexplain.ReasonHerdrOnly {
		t.Fatalf("explained as %q", got)
	}
	later := herdrT0.Add(time.Second)
	s.SetHerdr(reading(HerdrBlocked, false, later), later)
	if got := s.publishedStatus(later); got != "" {
		t.Fatalf("unfollowed herdr published %q, want unknown", got)
	}
	if got := s.ExplainStatus(later).Reason; got != statusexplain.ReasonObservationExpired {
		t.Fatalf("unfollowed herdr explained as %q", got)
	}
}
