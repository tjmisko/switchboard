package claude

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// Step 11 of docs/askuserquestion-model-plan.md §4, at the adapter. The daemon
// now persists a writer's whole open set rather than one prompt per writer, and
// these pin what the adapter must do with it: rebuild the set, keep the two
// clocks apart, and never restore a prompt that folds to no colour.

// restoredWith builds the compatibility block a restore is handed, with the
// per-call records the persisted schema carries.
func restoredWith(sessionID, transcriptPath string, sets map[string][]PendingPrompt) Compatibility {
	writers := make(map[string]PendingPrompt, len(sets))
	for writer := range sets {
		writers[writer] = PendingPrompt{}
	}
	return Compatibility{
		SessionID: sessionID, Transcript: transcriptPath,
		Status: agentgraph.LegacyPermission, Pending: writers, PendingSets: sets,
	}
}

func TestRestoreRebuildsAWritersWholeOpenSet(t *testing.T) {
	t.Run("should restore every prompt a writer held when the block carries the set", func(t *testing.T) {
		o, root, now := newTestObserver(t)
		defer o.Close()
		onset := now.Add(-2 * time.Minute)
		restored := restoredWith(root.ProviderSessionID, root.Transcript, map[string][]PendingPrompt{
			"": {
				{Tool: "Bash", InputHash: "call-a", Attention: agentgraph.AttentionApproval, Since: onset},
				{Tool: "Bash", InputHash: "call-b", Attention: agentgraph.AttentionApproval, Since: onset.Add(time.Second)},
			},
		})
		if _, err := o.Restore(root, restored, now); err != nil {
			t.Fatal(err)
		}

		prompts := o.Projection(root.Key()).PendingSets[""]
		if len(prompts) != 2 {
			t.Fatalf("restored %d prompts, want both: %+v", len(prompts), prompts)
		}
		if !prompts[0].Since.Equal(onset) || !prompts[1].Since.Equal(onset.Add(time.Second)) {
			t.Errorf("restored onsets = %v/%v, want the persisted ones oldest-first", prompts[0].Since, prompts[1].Since)
		}
		for _, prompt := range prompts {
			if prompt.Residual {
				t.Errorf("prompt %+v came back residual; a record names one call and must be able to latch it", prompt)
			}
			if prompt.Latch != CallLatchUnbound || prompt.CallID != "" {
				t.Errorf("prompt %+v came back with a latch; the call id is re-earned, never persisted", prompt)
			}
		}
	})

	t.Run("should hold a restored red against transcript evidence older than the restart", func(t *testing.T) {
		// The second clock, and the reason there are two. A record's Since is its own
		// pre-restart onset, which is what dates it against its writer's transcript
		// for the latch — but every WHOLE-FILE rule must run from the restart instant
		// instead. Dating those from the onset would let any entry the writer wrote
		// before the daemon went down read as "this writer resumed" and clear a red
		// that was live across the restart, which is a missed RED on the first tick
		// after every restart.
		o, root, now := newTestObserver(t)
		defer o.Close()
		onset := now.Add(-10 * time.Minute)
		appendClaudeLine(t, root.Transcript, `{"type":"assistant","timestamp":"`+
			onset.Add(time.Minute).Format(time.RFC3339Nano)+`","message":{"role":"assistant","content":[]}}`)
		restored := restoredWith(root.ProviderSessionID, root.Transcript, map[string][]PendingPrompt{
			"": {{Tool: "AskUserQuestion", InputHash: "ask-1", Attention: agentgraph.AttentionUserInput, Since: onset}},
		})
		if _, err := o.Restore(root, restored, now); err != nil {
			t.Fatal(err)
		}

		observation, err := o.Observe(context.Background(), root, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		assertSummary(t, observation, now.Add(time.Second), agentgraph.LegacyPermission, agentgraph.AttentionUserInput)
	})

	t.Run("should restore a record with no attention as its tool's own kind", func(t *testing.T) {
		// Never to none. AttentionState.Valid() accepts "none", and a prompt that
		// folds to no attention is a red that comes back green — so the repair
		// rejects it along with an unset or unknown value.
		o, root, now := newTestObserver(t)
		defer o.Close()
		restored := restoredWith(root.ProviderSessionID, root.Transcript, map[string][]PendingPrompt{
			"": {{Tool: "AskUserQuestion", InputHash: "ask-1", Attention: agentgraph.AttentionNone, Since: now.Add(-time.Minute)}},
			"child-a": {
				{Tool: "Bash", InputHash: "call-a", Attention: agentgraph.AttentionState("something-newer"), Since: now.Add(-time.Minute)},
			},
		})
		observation, err := o.Restore(root, restored, now)
		if err != nil {
			t.Fatal(err)
		}

		sets := o.Projection(root.Key()).PendingSets
		if got := sets[""][0].Attention; got != agentgraph.AttentionUserInput {
			t.Errorf("restored attention = %q, want the question's own kind", got)
		}
		if got := sets["child-a"][0].Attention; got != agentgraph.AttentionApproval {
			t.Errorf("restored attention = %q, want approval for an unrecognized value", got)
		}
		assertSummary(t, observation, now, agentgraph.LegacyPermission, agentgraph.AttentionApproval)
	})

	t.Run("should keep a legacy block restoring exactly as it always did", func(t *testing.T) {
		// Backward compatibility at the adapter: a block with no records carries one
		// prompt per writer, and that prompt is RESIDUAL — it stands for the writer's
		// leftover red rather than for a call, so nothing may bind an id to it.
		o, root, now := newTestObserver(t)
		defer o.Close()
		restored := Compatibility{
			SessionID: root.ProviderSessionID, Transcript: root.Transcript,
			Status:  agentgraph.LegacyPermission,
			Pending: map[string]PendingPrompt{"": {Since: now.Add(-time.Minute)}},
		}
		if _, err := o.Restore(root, restored, now); err != nil {
			t.Fatal(err)
		}

		prompts := o.Projection(root.Key()).PendingSets[""]
		if len(prompts) != 1 || !prompts[0].Residual {
			t.Fatalf("legacy restore = %+v, want exactly one residual prompt", prompts)
		}
		if got := prompts[0].Attention; got != agentgraph.AttentionApproval {
			t.Errorf("legacy restore attention = %q, want approval — the shape it has always had", got)
		}
	})
}
