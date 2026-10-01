package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

func asyncQuestionItem(t *testing.T, method string) rpcNotification {
	t.Helper()
	return rpcNotification{Method: method, Params: mustJSON(t, map[string]any{
		"threadId": "root", "turnId": "turn", "item": map[string]any{
			"type": "agentMessage", "id": "question", "delivery": "async",
			"questions": []any{map[string]any{"title": "Choose a color", "options": []string{"red", "blue"}}},
		},
	})}
}

func TestAsyncQuestionStaysRedThroughWorkCompletionAndSnapshots(t *testing.T) {
	observer, key := newWaitObserver(t, time.Second, nil)
	for _, method := range []string{"item/started", "item/completed"} {
		applyWaitNote(t, observer, asyncQuestionItem(t, method))
		if node := waitNode(t, observer, key); node.Runtime != agentgraph.RuntimeActive || node.Attention != agentgraph.AttentionUserInput {
			t.Fatalf("async question did not request attention during work: %#v", node)
		}
	}
	applyWaitNote(t, observer, rpcNotification{Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
		"threadId": "root", "status": map[string]any{"type": "idle"},
	})})
	applyWaitNote(t, observer, rpcNotification{Method: "turn/completed", Params: mustJSON(t, map[string]any{
		"threadId": "root", "turn": map[string]any{"id": "turn", "status": "completed"},
	})})
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)

	snapshot := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "idle"}}, nil, 32)
	observer.installSnapshot(1, key, "root", snapshot, nil, false, nil)
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)

	applyWaitNote(t, observer, rpcNotification{Method: "item/started", Params: mustJSON(t, map[string]any{
		"threadId": "root", "item": map[string]any{"type": "userMessage", "id": "answer"},
	})})
	assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
	// A delayed duplicate completion must not reopen the answered question.
	applyWaitNote(t, observer, asyncQuestionItem(t, "item/completed"))
	assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
}

func TestQuestionMetadataWithoutAsyncDeliveryStillRequestsAttention(t *testing.T) {
	observer, key := newWaitObserver(t, time.Second, nil)
	applyWaitNote(t, observer, rpcNotification{Method: "item/completed", Params: mustJSON(t, map[string]any{
		"threadId": "root", "item": map[string]any{
			"type": "agentMessage", "id": "question", "questions": []any{map[string]any{"title": "Pick one"}},
		},
	})})
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)
}

func TestNonblockingRequestSurvivesRuntimeFlagsAndSameConnectionSnapshot(t *testing.T) {
	observer, key := newWaitObserver(t, time.Second, nil)
	applyWaitNote(t, observer, rpcNotification{Method: "thread/settings/updated", Params: mustJSON(t, map[string]any{
		"threadId": "root", "threadSettings": map[string]any{"approvalsReviewer": "auto_review"},
	})})
	// Missing blocking metadata is also a question, not evidence of no attention.
	applyWaitNote(t, observer, rpcNotification{ID: json.RawMessage(`"input"`), Method: "item/tool/requestUserInput", Params: mustJSON(t, map[string]any{
		"threadId": "root", "turnId": "turn", "itemId": "question",
	})})
	applyWaitNote(t, observer, rpcNotification{Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
		"threadId": "root", "status": map[string]any{"type": "active", "activeFlags": []string{}},
	})})
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)
	snapshot := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "active"}}, nil, 32)
	observer.installSnapshot(1, key, "root", snapshot, nil, false, nil)
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)
	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`"input"`)))
	assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
}

func TestAsyncDismissalDoesNotResolveUnrelatedRPCInput(t *testing.T) {
	observer, key := newWaitObserver(t, time.Second, nil)
	applyWaitNote(t, observer, asyncQuestionItem(t, "item/completed"))
	applyWaitNote(t, observer, rpcNotification{ID: json.RawMessage(`"blocking"`), Method: "item/tool/requestUserInput", Params: mustJSON(t, map[string]any{
		"threadId": "root", "turnId": "turn", "itemId": "other", "isBlocking": true,
	})})
	observer.DismissAsyncQuestions(key, "retired-thread")
	observer.DismissAsyncQuestions(key, "root")
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)
	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`"blocking"`)))
	assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
}

func TestHistoricalQuestionsAreNotReopenedAndQuestionTextIsDiscarded(t *testing.T) {
	var item rpcItem
	if err := json.Unmarshal([]byte(`{"type":"agentMessage","id":"old","delivery":"async","questions":[{"title":"private question","options":["private answer"]}]}`), &item); err != nil {
		t.Fatal(err)
	}
	state := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "idle"}, Turns: []rpcTurn{{ID: "old-turn", Items: []rpcItem{item}}}}, nil, 32)
	if state.hasHumanAttention() {
		t.Fatal("historical question was reopened")
	}
	questions, err := json.Marshal(item.Questions)
	if err != nil || string(questions) != "[{}]" {
		t.Fatalf("question content retained: %s, err=%v", questions, err)
	}
	state.applyAsyncInputItem("root", item)
	clone := state.clone()
	clone.clearAsyncInputs("root")
	if !state.hasHumanAttention() {
		t.Fatal("clearing a clone erased the live question")
	}
	state.resetWaitOwnership()
	if state.hasHumanAttention() {
		t.Fatal("connection reset retained async attention")
	}
}

func TestDuplicateUserMessageDoesNotDismissNewQuestion(t *testing.T) {
	state := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "active"}}, nil, 32)
	question := rpcItem{Type: "agentMessage", ID: "first", Questions: []struct{}{{}}}
	state.applyAsyncInputItem("root", question)
	answer := rpcItem{Type: "userMessage", ID: "answer"}
	state.applyAsyncInputItem("root", answer)
	question.ID = "second"
	state.applyAsyncInputItem("root", question)
	snapshot := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "active"}}, nil, 32)
	snapshot.preserveInputAttention(state)
	snapshot.applyAsyncInputItem("root", answer)
	if !snapshot.hasHumanAttention() {
		t.Fatal("duplicate user-message completion dismissed a newer question")
	}
	answer.ID = "next-answer"
	snapshot.applyAsyncInputItem("root", answer)
	if snapshot.hasHumanAttention() {
		t.Fatal("new user message did not dismiss the question")
	}
}
