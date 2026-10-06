package claude

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/tailcache"
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

// #98 operation counts: an unchanged poll stats each file once and reads none;
// a hook makes the next poll re-read the root's files; an append re-reads only
// the file that moved.
func TestObserveShouldStatEachFileOnceAndReadNoneWhenThePollIsUnchanged(t *testing.T) {
	t.Cleanup(tailcache.SetDefault(tailcache.New(tailcache.OS{}, 0)))
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now.Add(time.Second)})
	appendClaudeLine(t, root.Transcript, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"go"}}`, now.Add(time.Second).Format(time.RFC3339Nano)))
	writeClaudeChild(t, claudeSubagentDir(root), "first", "general-purpose", "work", "", now)
	writeClaudeChild(t, claudeSubagentDir(root), "second", "general-purpose", "work", "", now)
	cache := tailcache.Default()

	observe := func(at time.Time) tailcache.Stats {
		t.Helper()
		before := cache.Stats()
		if _, err := o.Observe(context.Background(), root, at); err != nil {
			t.Fatal(err)
		}
		after := cache.Stats()
		return tailcache.Stats{Stats: after.Stats - before.Stats, Reads: after.Reads - before.Reads, Hits: after.Hits - before.Hits}
	}
	// The main transcript's runtime signal, and each child's meta and transcript.
	const files = 5
	if work := observe(now.Add(2 * time.Second)); work.Reads != files {
		t.Fatalf("first poll = %+v, want every file read once", work)
	}
	if work := observe(now.Add(3 * time.Second)); work.Stats != files || work.Reads != 0 {
		t.Fatalf("unchanged poll = %+v, want %d stats and no read", work, files)
	}

	o.ApplyHook(HookSignal{Root: root, Event: "PreToolUse", At: now.Add(4 * time.Second)})
	if work := observe(now.Add(5 * time.Second)); work.Reads != files {
		t.Fatalf("poll after a hook = %+v, want the root's files re-read", work)
	}

	appendClaudeLine(t, filepath.Join(claudeSubagentDir(root), "agent-first.jsonl"), `{"type":"assistant","message":{"role":"assistant","stop_reason":null}}`)
	if work := observe(now.Add(6 * time.Second)); work.Stats != files || work.Reads != 1 {
		t.Fatalf("poll after one child appended = %+v, want only that child re-read", work)
	}
}

// #98 criterion 4: transcript-inferred idle waits out its quiet window, so a
// stop summary between hooks does not split an active interval, while the
// explicit Stop hook publishes idle on the very next tick.
func TestObserveShouldDelayOnlyInferredIdleWhenAStopIsExplicit(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now})
	appendClaudeLine(t, root.Transcript, fmt.Sprintf(`{"type":"system","subtype":"stop_hook_summary","preventedContinuation":false,"timestamp":%q}`, now.Add(time.Second).Format(time.RFC3339Nano)))
	inferred := now.Add(2 * time.Second)
	obs, err := o.Observe(context.Background(), root, inferred)
	if err != nil {
		t.Fatal(err)
	}
	assertSummary(t, obs, inferred, agentgraph.LegacyWorking, agentgraph.AttentionNone)

	stopAt := now.Add(3 * time.Second)
	o.ApplyHook(HookSignal{Root: root, Event: "Stop", At: stopAt})
	next := stopAt.Add(time.Millisecond)
	obs, err = o.Observe(context.Background(), root, next)
	if err != nil {
		t.Fatal(err)
	}
	assertSummary(t, obs, next, agentgraph.LegacyIdle, agentgraph.AttentionNone)
}
