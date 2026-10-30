package label

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestDisplayNamesUseSameFiveWordPolicyAcrossProviders(t *testing.T) {
	const native = "sb-fix-unicode-project-titles-across-providers-now"
	const want = "sb Fix Unicode Project Titles Across"
	writeSessionFile(t, 4242, native)
	for _, agent := range []string{state.AgentKindClaude, state.AgentKindCodex, state.AgentKindPi} {
		s := state.Session{PID: 4242, Agent: agent, CWD: "/projects/switchboard"}
		if agent == state.AgentKindCodex {
			s.Codex = &state.AgentInfo{SessionID: "thread"}
			s.DisplayName = &state.DisplayName{Value: native, ConversationID: "thread", Origin: state.DisplayNameGenerated}
		}
		if agent == state.AgentKindPi {
			s.Pi = &state.AgentInfo{SessionID: "pi-session"}
			s.DisplayName = &state.DisplayName{Value: native, ConversationID: "pi-session", Origin: state.DisplayNameNative}
		}
		for _, remote := range []bool{false, true} {
			s.Remote, s.ResolvedName = remote, native
			var cache NameCache
			if got := cache.DisplayName(projectname.DefaultConfig(), s).String(); got != want {
				t.Errorf("%s remote=%v: %q, want %q", agent, remote, got, want)
			}
		}
	}
}

func TestUnnamedPiUsesOnlyProject(t *testing.T) {
	writeSessionFile(t, 4242, "unrelated-Claude-name")
	s := state.Session{PID: 4242, Agent: state.AgentKindPi, CWD: "/projects/中国地理",
		Pi:          &state.AgentInfo{SessionID: "new-session"},
		DisplayName: &state.DisplayName{Value: "stale-name", ConversationID: "old-session", Origin: state.DisplayNameNative},
		Wezterm:     &state.WeztermInfo{WindowTitle: "π - 中国地理"}}
	if got := DisplayName(projectname.DefaultConfig(), s).String(); got != "中国地理" {
		t.Errorf("unnamed Pi = %q", got)
	}
	if got := RawName(s); got != "中国地理" {
		t.Errorf("Pi legacy name borrowed provider chrome: %q", got)
	}
}
