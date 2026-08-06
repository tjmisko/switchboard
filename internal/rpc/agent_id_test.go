package rpc

import (
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// T14 — the two id namespaces must join. transcript.Subagent.AgentID and
// history.Event.AgentID hold the PREFIX-STRIPPED id (both derived from the
// agent-<id>.* file names by the fanout Observer); rpc.Request.AgentID holds
// whatever the hook sends, which nobody has read off a live payload yet (plan
// T1). normalizeAgentID makes the two agree whichever spelling arrives, so a map
// keyed by the hook value cannot silently fail to join the Observer's seen-set.
func TestNormalizeAgentID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// The shape the Observer writes, and the shape we expect the hook to
			// send. Nothing to do.
			name: "should leave the id unchanged when the hook sends the bare form",
			in:   "a158b13da3d13b0ea",
			want: "a158b13da3d13b0ea",
		},
		{
			// The other half of T1's coin flip: the hook sends the file spelling.
			name: "should strip the prefix when the hook sends the file-name form",
			in:   "agent-a158b13da3d13b0ea",
			want: "a158b13da3d13b0ea",
		},
		{
			// Empty means MAIN THREAD (Claude Code omits agent_id off a subagent),
			// so it is a discriminator and must survive untouched.
			name: "should stay empty when the hook omits agent_id for the main thread",
			in:   "",
			want: "",
		},
		{
			// Subagent ids are themselves `a`-initial, so a doubled prefix is a
			// plausible wire value. One strip, never two — the remainder is the id.
			name: "should strip only once when the value carries a doubled prefix",
			in:   "agent-agent-a158b13da3d13b0ea",
			want: "agent-a158b13da3d13b0ea",
		},
		{
			// A named teammate's file is agent-aauth-tests-7152e6a858d30551, so the
			// bare id embeds hyphens and a name. Nothing here may be touched.
			name: "should leave a name-style id intact when the id embeds a teammate name",
			in:   "aauth-tests-7152e6a858d30551",
			want: "aauth-tests-7152e6a858d30551",
		},
		{
			name: "should strip the prefix once when a name-style id arrives prefixed",
			in:   "agent-aauth-tests-7152e6a858d30551",
			want: "aauth-tests-7152e6a858d30551",
		},
		{
			// Degenerate, but decisive: stripping this to "" would re-attribute a
			// subagent's hook to the main thread — the invisible failure T14 exists
			// to close. Non-empty in, non-empty out.
			name: "should leave a bare prefix intact when stripping would empty the id",
			in:   "agent-",
			want: "agent-",
		},
		{
			// Only a LEADING prefix is meaningful; the id is otherwise opaque.
			name: "should leave the id unchanged when agent- appears anywhere but the front",
			in:   "a158-agent-b13da3d",
			want: "a158-agent-b13da3d",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeAgentID(tc.in); got != tc.want {
				t.Errorf("normalizeAgentID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The normalization is worthless if it does not sit on the path a hook actually
// travels. handleHook is the single choke point, so the identity it hands to every
// downstream consumer must already be bare — observable today through the T1
// hook-identity log line, which reports req.AgentID verbatim.
func TestHandleHookNormalizesAgentID(t *testing.T) {
	t.Run("should log the bare id when a PermissionRequest arrives prefixed", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p"}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "Bash",
			SessionID: "5318eb5b-79df", AgentID: "agent-af5bd126402ac16c7", AgentType: "general-purpose"})

		out := buf.String()
		if !strings.Contains(out, `agent_id="af5bd126402ac16c7"`) {
			t.Errorf("want the prefix stripped at the boundary, got:\n%s", out)
		}
		if strings.Contains(out, `agent_id="agent-`) {
			t.Errorf("the raw hook spelling reached a consumer unnormalized:\n%s", out)
		}
	})

	t.Run("should log the same bare id when the hook sends the already-bare form", func(t *testing.T) {
		// The point of the normalization is that T1's eventual answer stops
		// mattering: both spellings must converge on one key.
		_, s := redSession(t, "Bash", 4, siblingResultOnly)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			AgentID: "aa83942381ce15c04"})

		if !strings.Contains(buf.String(), `agent_id="aa83942381ce15c04"`) {
			t.Errorf("a bare id must pass through untouched, got:\n%s", buf.String())
		}
	})

	t.Run("should keep the id empty when the hook fires from the main thread", func(t *testing.T) {
		_, s := redSession(t, "Bash", 4, siblingResultOnly)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		if !strings.Contains(buf.String(), `agent_id=""`) {
			t.Errorf("empty means main thread and must survive the boundary, got:\n%s", buf.String())
		}
	})
}
