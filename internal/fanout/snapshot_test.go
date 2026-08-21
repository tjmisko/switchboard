package fanout

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestObserveReturnsDetachedStructuredChildrenInStableOrder(t *testing.T) {
	e := newEnv(t)
	runningPath := writeSub(t, e.subdir, "z-running", `{"agentType":"Explore","name":"searcher","description":"map packages","spawnDepth":1,"toolUseId":"toolu_z"}`, "")
	donePath := writeSub(t, e.subdir, "a-done", metaClassic(1, "toolu_a"), "end_turn")
	now := time.Now().Round(time.Second)
	obs := NewObserver(e.historyDir)

	got, err := obs.Observe(Root{
		SessionID: e.sid, Transcript: e.transcript,
		PID: e.sess.PID, Agent: e.sess.Agent, CWD: e.sess.CWD,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Snapshot.Complete || !got.Snapshot.ObservedAt.Equal(now) {
		t.Fatalf("snapshot metadata = %+v", got.Snapshot)
	}
	if got.Snapshot.InFlight != 1 {
		t.Fatalf("InFlight = %d, want 1", got.Snapshot.InFlight)
	}
	if ids := childIDs(got.Snapshot.Children); !reflect.DeepEqual(ids, []string{"a-done", "z-running"}) {
		t.Fatalf("child order = %v, want stable id order", ids)
	}
	done := got.Snapshot.Children[0]
	if done.Lifecycle != LifecycleCompleted || done.TranscriptPath != donePath || done.CompletedAt.IsZero() {
		t.Fatalf("completed child = %+v", done)
	}
	running := got.Snapshot.Children[1]
	if running.Lifecycle != LifecycleRunning || running.TranscriptPath != runningPath ||
		running.Nickname != "searcher" || running.AgentType != "Explore" || running.Description != "map packages" {
		t.Fatalf("running child = %+v", running)
	}

	// The caller owns every returned slice. Mutation must not affect the next
	// observation or the observer's restart/idempotence bookkeeping.
	got.Snapshot.Children[0].ID = "mutated"
	again, err := obs.Observe(Root{SessionID: e.sid, Transcript: e.transcript}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if ids := childIDs(again.Snapshot.Children); !reflect.DeepEqual(ids, []string{"a-done", "z-running"}) {
		t.Fatalf("observer retained caller mutation: %v", ids)
	}
	if len(again.Events) != 0 {
		t.Fatalf("unchanged second observation re-emitted events: %+v", again.Events)
	}
	if !again.Snapshot.Children[0].CompletedAt.Equal(done.CompletedAt) {
		t.Fatalf("stable completion changed from %v to %v", done.CompletedAt, again.Snapshot.Children[0].CompletedAt)
	}
}

func TestObserveDistinguishesPendingAndStaleInterruptedChildren(t *testing.T) {
	e := newEnv(t)
	meta := `{"agentType":"general-purpose","description":"not started","spawnDepth":1}`
	if err := os.WriteFile(filepath.Join(e.subdir, "agent-pending.meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	stalePath := writeSub(t, e.subdir, "stale", metaClassic(1, "toolu_stale"), "")
	now := time.Now().Round(time.Second)
	old := now.Add(-time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}
	obs := NewObserver(e.historyDir)
	obs.SetStaleCap(30 * time.Minute)

	got, err := obs.Observe(Root{SessionID: e.sid, Transcript: e.transcript}, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := childrenByID(got.Snapshot.Children)
	if byID["pending"].Lifecycle != LifecyclePending {
		t.Fatalf("meta-only child lifecycle = %q, want pending", byID["pending"].Lifecycle)
	}
	if stale := byID["stale"]; stale.Lifecycle != LifecycleInterrupted || stale.SuspectReason != SuspectStaleQuiescent {
		t.Fatalf("stale child = %+v, want interrupted with suspect reason", stale)
	}
	foundReason := false
	for _, event := range got.Events {
		if event.AgentID == "stale" && event.Reason == string(SuspectStaleQuiescent) {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("stale legacy stop event lost suspect reason: %+v", got.Events)
	}
}

func TestObserveIncludesWorkflowAssociationAndProgress(t *testing.T) {
	e := newEnv(t)
	runDir := writeWorkflowRun(t, e, "wf_structured-1", "review")
	writeJournal(t, runDir, []string{wfStarted("w2"), wfStarted("w1"), wfResult("w1")})
	writeWorkflowAgent(t, runDir, "w1")
	writeWorkflowAgent(t, runDir, "w2")
	obs := NewObserver(e.historyDir)
	now := time.Now().Round(time.Second)

	got, err := obs.Observe(Root{SessionID: e.sid, Transcript: e.transcript}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Snapshot.Workflows) != 1 {
		t.Fatalf("workflows = %+v", got.Snapshot.Workflows)
	}
	wf := got.Snapshot.Workflows[0]
	if wf.RunID != "wf_structured-1" || wf.Name != "review" || wf.AgentsStarted != 2 || wf.AgentsDone != 1 || wf.InFlight != 1 {
		t.Fatalf("workflow = %+v", wf)
	}
	byID := childrenByID(got.Snapshot.Children)
	if child := byID["w1"]; child.WorkflowRunID != wf.RunID || child.WorkflowName != wf.Name || child.Lifecycle != LifecycleCompleted {
		t.Fatalf("completed workflow child = %+v", child)
	}
	if child := byID["w2"]; child.WorkflowRunID != wf.RunID || child.Lifecycle != LifecycleRunning {
		t.Fatalf("running workflow child = %+v", child)
	}
}

func TestObserveKeepsAsyncLaunchAcknowledgementRunning(t *testing.T) {
	e := newEnv(t)
	obs := NewObserver(e.historyDir)
	now := time.Now().Round(time.Second)
	root := Root{SessionID: e.sid, Transcript: e.transcript}
	if _, err := obs.Observe(root, now); err != nil { // prime cursor
		t.Fatal(err)
	}
	appendLine(t, e.transcript, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_async","name":"Agent","input":{"subagent_type":"Explore"}}]}}`)
	appendLine(t, e.transcript, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_async","content":"Async agent launched successfully. agentId: async"}]}}`)
	writeSub(t, e.subdir, "async", metaClassic(1, "toolu_async"), "")

	got, err := obs.Observe(root, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	child := childrenByID(got.Snapshot.Children)["async"]
	if child.Lifecycle != LifecycleRunning || !child.Background || got.Snapshot.InFlight != 1 {
		t.Fatalf("async launch acknowledgement closed child: snapshot=%+v child=%+v", got.Snapshot, child)
	}
}

func TestObserveResetsParentCursorAfterTruncation(t *testing.T) {
	e := newEnv(t)
	obs := NewObserver(e.historyDir)
	now := time.Now().Round(time.Second)
	root := Root{SessionID: e.sid, Transcript: e.transcript}
	if _, err := obs.Observe(root, now); err != nil {
		t.Fatal(err)
	}
	appendLine(t, e.transcript, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_reset","name":"Task","input":{"subagent_type":"Explore","description":"intentionally long parent record to force a smaller replacement"}}]}}`)
	writeSub(t, e.subdir, "reset", metaClassic(1, "toolu_reset"), "")
	first, err := obs.Observe(root, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := childrenByID(first.Snapshot.Children)["reset"].Lifecycle; got != LifecycleRunning {
		t.Fatalf("initial lifecycle = %q, want running", got)
	}

	// Simulate /clear or compaction replacing the parent transcript with a
	// shorter file. The cursor must restart at zero and see the completion.
	replacement := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_reset"}]}}` + "\n"
	if err := os.WriteFile(e.transcript, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := obs.Observe(root, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := childrenByID(second.Snapshot.Children)["reset"].Lifecycle; got != LifecycleCompleted {
		t.Fatalf("post-truncation lifecycle = %q, want completed", got)
	}
}

func TestObserveToleratesPartialMetadata(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.subdir, "agent-partial.meta.json"), []byte(`{"agentType":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.subdir, "agent-partial.jsonl"), []byte(`{"type":"assistant","message":{"role":"assistant","stop_reason":null}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := NewObserver(e.historyDir).Observe(Root{SessionID: e.sid, Transcript: e.transcript}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	child := childrenByID(got.Snapshot.Children)["partial"]
	if child.ID != "partial" || child.AgentType != "" || child.Lifecycle != LifecycleRunning {
		t.Fatalf("partial metadata child = %+v", child)
	}
}

func childIDs(children []Child) []string {
	ids := make([]string, len(children))
	for i := range children {
		ids[i] = children[i].ID
	}
	return ids
}

func childrenByID(children []Child) map[string]Child {
	byID := make(map[string]Child, len(children))
	for _, child := range children {
		byID[child.ID] = child
	}
	return byID
}
