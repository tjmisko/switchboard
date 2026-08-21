package claude

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

func TestCanonicalShadowCasesAreDeterministicAndLegacyEquivalent(t *testing.T) {
	first := CanonicalShadowCases()
	second := CanonicalShadowCases()
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("shadow cases lengths = %d, %d", len(first), len(second))
	}
	first[0].Hooks[0].Event = "mutated"
	if second[0].Hooks[0].Event == "mutated" {
		t.Fatal("CanonicalShadowCases returned shared hook storage")
	}

	for _, fixture := range second {
		t.Run(fixture.Name, func(t *testing.T) {
			o, root, now := newTestObserver(t)
			defer o.Close()
			var observation agentgraph.Observation
			for i, hook := range fixture.Hooks {
				hook.Root = root
				hook.At = now.Add(time.Duration(i) * time.Second)
				observation = o.ApplyHook(hook).Observation
			}
			comparison := CompareShadow(fixture.LegacyStatus, observation, agentgraph.Summary{}, now.Add(time.Duration(len(fixture.Hooks)-1)*time.Second))
			if !comparison.Match || comparison.GraphStatus != fixture.LegacyStatus {
				t.Fatalf("comparison = %+v", comparison)
			}
			if comparison.LiveChildren != fixture.LiveChildren || comparison.WaitingNodes != fixture.WaitingNodes {
				t.Fatalf("comparison counts = %+v, want children=%d waiting=%d", comparison, fixture.LiveChildren, fixture.WaitingNodes)
			}
		})
	}
}
