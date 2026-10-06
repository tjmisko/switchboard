package claude

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// #98: a failed scan is unavailable, carrying the prior graph to its original
// deadline; a successful scan is usable; recovery is usable at once.
func TestObserveShouldReportUnavailableCarryingThePriorGraphWhenTheScanFailsAndUsableWhenItRecovers(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now.Add(time.Second)})
	writeClaudeChild(t, claudeSubagentDir(root), "live", "general-purpose", "work", "", now)
	scannedAt := now.Add(2 * time.Second)
	usable, err := o.Observe(context.Background(), root, scannedAt)
	if err != nil || usable.Outcome != agentgraph.OutcomeUsable {
		t.Fatalf("successful scan = %q, %v; want usable", usable.Outcome, err)
	}

	restore := breakFanoutScan(t, root)
	held, err := o.Observe(context.Background(), root, scannedAt.Add(10*time.Second))
	if err == nil {
		t.Fatal("a failed fanout scan returned no error")
	}
	if held.Outcome != agentgraph.OutcomeUnavailable {
		t.Fatalf("failed scan outcome = %q, want unavailable", held.Outcome)
	}
	if held.RootID != root.ProviderSessionID || len(held.Nodes) == 0 ||
		!held.ObservedAt.Equal(usable.ObservedAt) || !held.FreshUntil.Equal(usable.FreshUntil) {
		t.Fatalf("failed scan carried [%v, %v) root=%q nodes=%d, want the prior graph's [%v, %v)",
			held.ObservedAt, held.FreshUntil, held.RootID, len(held.Nodes), usable.ObservedAt, usable.FreshUntil)
	}
	if agentgraph.OutcomeOf(held, err) != agentgraph.OutcomeUnavailable {
		t.Fatal("the coordinator would classify the held graph as usable")
	}

	restore()
	recoveredAt := usable.FreshUntil.Add(time.Second)
	recovered, err := o.Observe(context.Background(), root, recoveredAt)
	if err != nil || recovered.Outcome != agentgraph.OutcomeUsable || !recovered.ObservedAt.Equal(recoveredAt) {
		t.Fatalf("recovered scan = %q at %v, %v; want usable dated %v", recovered.Outcome, recovered.ObservedAt, err, recoveredAt)
	}
}

func TestObserveShouldReportUnavailableWhenTheFirstScanFailsWithNothingToHold(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	breakFanoutScan(t, root)
	observation, err := o.Observe(context.Background(), root, now)
	if err == nil {
		t.Fatal("a failed fanout scan returned no error")
	}
	if got := agentgraph.OutcomeOf(observation, err); got != agentgraph.OutcomeUnavailable || observation.RootID != "" {
		t.Fatalf("first failed scan = %q root=%q, want unavailable with nothing carried", got, observation.RootID)
	}
}
