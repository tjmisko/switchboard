package state

import "testing"

// AgentBlock allocates the right per-agent block, records the kind once, and is
// idempotent on repeat calls; Enrichment reads back whichever block the kind
// selects.
func TestAgentBlockAndEnrichment(t *testing.T) {
	t.Run("codex routes to codex block and stamps kind", func(t *testing.T) {
		s := &Session{PID: 1}
		info := s.AgentBlock(AgentKindCodex)
		info.Status = "working"

		if s.Agent != AgentKindCodex {
			t.Errorf("agent = %q, want codex", s.Agent)
		}
		if s.Claude != nil {
			t.Errorf("claude block allocated for a codex hook: %+v", s.Claude)
		}
		if s.Codex == nil || s.Codex.Status != "working" {
			t.Errorf("codex block = %+v, want status=working", s.Codex)
		}
		// Idempotent: a second call returns the same pointer, not a fresh block.
		if again := s.AgentBlock(AgentKindCodex); again != s.Codex {
			t.Errorf("AgentBlock re-allocated the codex block")
		}
		if got := s.Enrichment(); got != s.Codex {
			t.Errorf("Enrichment() = %p, want the codex block %p", got, s.Codex)
		}
	})

	t.Run("default kind routes to claude block", func(t *testing.T) {
		s := &Session{PID: 2}
		s.AgentBlock(AgentKindClaude).Status = "idle"
		if s.Codex != nil {
			t.Errorf("codex block allocated for a claude hook: %+v", s.Codex)
		}
		if s.Enrichment() != s.Claude {
			t.Errorf("Enrichment() did not return the claude block")
		}
	})

	t.Run("no enrichment yet returns nil", func(t *testing.T) {
		s := Session{PID: 3}
		if got := s.Enrichment(); got != nil {
			t.Errorf("Enrichment() = %+v, want nil before any hook", got)
		}
	})

	t.Run("unset kind falls back to whichever block exists", func(t *testing.T) {
		s := Session{PID: 4, Codex: &AgentInfo{Status: "permission"}}
		if got := s.Enrichment(); got == nil || got.Status != "permission" {
			t.Errorf("Enrichment() = %+v, want the codex block by fallback", got)
		}
	})
}
