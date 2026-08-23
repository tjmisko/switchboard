package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

func TestReviewerModesAndGuardianSourceAreParsedConservatively(t *testing.T) {
	state := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "active"}}, nil, 32)
	for value, want := range map[string]reviewerRoute{
		"user":              reviewerUser,
		"auto_review":       reviewerAuto,
		"guardian_subagent": reviewerAuto,
	} {
		if !state.setReviewer("root", value) || state.effectiveReviewer("root") != want {
			t.Errorf("reviewer %q mapped to %v, want %v", value, state.effectiveReviewer("root"), want)
		}
	}
	state.unknownEnum = false
	if !state.setReviewer("root", "future_reviewer") || state.effectiveReviewer("root") != reviewerUnknown || !state.unknownEnum {
		t.Fatal("unknown reviewer was not degraded safely")
	}

	guardian := mustJSON(t, map[string]any{"subAgent": map[string]any{"other": "guardian"}})
	notGuardian := mustJSON(t, map[string]any{"subAgent": map[string]any{"other": "research"}})
	if !isGuardianSource(guardian) || isGuardianSource(notGuardian) || isGuardianSource(json.RawMessage(`{"subAgent":{}}`)) {
		t.Fatal("guardian subagent source detection was not exact")
	}
	withGuardian := newGraphState(
		rpcThread{ID: "root", Status: rpcStatus{Type: "active", ActiveFlags: []string{"waitingOnApproval"}}},
		[]rpcThread{{ID: "guardian", ParentThreadID: "root", Source: guardian, Status: rpcStatus{Type: "active"}}},
		32,
	)
	if node := withGuardian.nodes["root"].node; node.Attention != agentgraph.AttentionNone || node.Runtime != agentgraph.RuntimeActive {
		t.Fatalf("guardian fallback did not classify parent wait as automatic: %#v", node)
	}
}

func TestServerRequestResolutionUsesExactStringOrIntegerID(t *testing.T) {
	observer, key := newWaitObserver(t, 30*time.Millisecond, nil)
	applyWaitNote(t, observer, rpcNotification{Method: "thread/settings/updated", Params: mustJSON(t, map[string]any{
		"threadId": "root", "threadSettings": map[string]any{"approvalsReviewer": "user"},
	})})
	applyWaitNote(t, observer, approvalWaitStatus(t))
	applyWaitNote(t, observer, approvalRequest(t, json.RawMessage(`1`), "item-number"))
	applyWaitNote(t, observer, approvalRequest(t, json.RawMessage(`"1"`), "item-string"))
	assertWaitAttention(t, observer, key, agentgraph.AttentionApproval)

	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`1`)))
	assertWaitAttention(t, observer, key, agentgraph.AttentionApproval)
	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`"missing"`)))
	assertWaitAttention(t, observer, key, agentgraph.AttentionApproval)
	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`"1"`)))
	node := waitNode(t, observer, key)
	if node.Attention != agentgraph.AttentionNone || node.Runtime != agentgraph.RuntimeUnknown {
		t.Fatalf("exact final resolution left mechanical gate authoritative: %#v", node)
	}
}

func TestAutoReviewOrderingAndTerminalOutcomesNeverPublishRed(t *testing.T) {
	for _, order := range []string{"wait_first", "review_first"} {
		for _, outcome := range []string{"allow", "deny", "timeout", "abort"} {
			t.Run(order+"_"+outcome, func(t *testing.T) {
				var diagnostics []WaitClassificationDiagnostic
				observer, key := newWaitObserver(t, 25*time.Millisecond, func(diagnostic WaitClassificationDiagnostic) {
					diagnostics = append(diagnostics, diagnostic)
				})
				start := autoReviewNote(t, "item/autoApprovalReview/started", outcome)
				if order == "wait_first" {
					applyWaitNote(t, observer, approvalWaitStatus(t))
					applyWaitNote(t, observer, approvalRequest(t, json.RawMessage(`"approval"`), "item"))
					assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
					applyWaitNote(t, observer, start)
				} else {
					applyWaitNote(t, observer, start)
					applyWaitNote(t, observer, approvalRequest(t, json.RawMessage(`"approval"`), "item"))
					applyWaitNote(t, observer, approvalWaitStatus(t))
				}
				applyWaitNote(t, observer, autoReviewNote(t, "item/autoApprovalReview/completed", outcome))
				time.Sleep(50 * time.Millisecond)
				node := waitNode(t, observer, key)
				if node.Attention != agentgraph.AttentionNone || node.Runtime != agentgraph.RuntimeActive {
					t.Fatalf("automatic %s review published human attention: %#v", outcome, node)
				}
				if order == "wait_first" {
					if len(diagnostics) != 1 || diagnostics[0].Source != "auto_review_event" || !diagnostics[0].SuppressedFalseRed {
						t.Fatalf("classification diagnostic = %#v", diagnostics)
					}
				}
			})
		}
	}
}

func TestUnknownApprovalBecomesHumanByDeadlineAndWaitsForExactResolution(t *testing.T) {
	observer, key := newWaitObserver(t, 25*time.Millisecond, nil)
	started := time.Now()
	applyWaitNote(t, observer, approvalWaitStatus(t))
	applyWaitNote(t, observer, approvalRequest(t, json.RawMessage(`"human"`), "item"))
	assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
	waitForWaitNode(t, observer, key, 250*time.Millisecond, func(node agentgraph.Node) bool {
		return node.Attention == agentgraph.AttentionApproval
	})
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("human approval classification took %v, want <= 500ms", elapsed)
	}
	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`"other"`)))
	assertWaitAttention(t, observer, key, agentgraph.AttentionApproval)
	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`"human"`)))
	assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
}

func TestUnknownMechanicalWaitExpiresGrayWithoutAttention(t *testing.T) {
	observer, key := newWaitObserver(t, 20*time.Millisecond, nil)
	applyWaitNote(t, observer, approvalWaitStatus(t))
	// Publication is suppressed during the classification window, so callers
	// still see the prior active state rather than a transient permission edge.
	if node := waitNode(t, observer, key); node.Runtime != agentgraph.RuntimeActive || node.Attention != agentgraph.AttentionNone {
		t.Fatalf("unclassified wait leaked an intermediate projection: %#v", node)
	}
	waitForWaitNode(t, observer, key, 200*time.Millisecond, func(node agentgraph.Node) bool {
		return node.Runtime == agentgraph.RuntimeUnknown && node.Attention == agentgraph.AttentionNone
	})
	observer.mu.Lock()
	observation := observer.roots[key].observation.Clone()
	observer.mu.Unlock()
	if summary := agentgraph.Reduce(observation, agentgraph.Summary{}, time.Now()); summary.LegacyStatus != "" {
		t.Fatalf("unknown mechanical ownership reduced to %q, want gray/unknown", summary.LegacyStatus)
	}
}

func TestUserInputMustBeBlockingAndNonResolvingToRequestAttention(t *testing.T) {
	for _, test := range []struct {
		name             string
		blocking         bool
		autoResolutionMs any
		want             agentgraph.AttentionState
	}{
		{name: "blocking", blocking: true, autoResolutionMs: nil, want: agentgraph.AttentionUserInput},
		{name: "nonblocking", blocking: false, autoResolutionMs: nil, want: agentgraph.AttentionNone},
		{name: "auto_resolving", blocking: true, autoResolutionMs: 100, want: agentgraph.AttentionNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer, key := newWaitObserver(t, 15*time.Millisecond, nil)
			applyWaitNote(t, observer, rpcNotification{Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
				"threadId": "root", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnUserInput"}},
			})})
			applyWaitNote(t, observer, rpcNotification{ID: json.RawMessage(`"input"`), Method: "item/tool/requestUserInput", Params: mustJSON(t, map[string]any{
				"threadId": "root", "turnId": "turn", "itemId": "item", "isBlocking": test.blocking, "autoResolutionMs": test.autoResolutionMs,
			})})
			if test.want == agentgraph.AttentionNone {
				time.Sleep(30 * time.Millisecond)
			}
			node := waitNode(t, observer, key)
			if node.Attention != test.want {
				t.Fatalf("attention = %s, want %s", node.Attention, test.want)
			}
			if test.want == agentgraph.AttentionNone && node.Runtime != agentgraph.RuntimeActive {
				t.Fatalf("non-human input request stopped active runtime: %#v", node)
			}
		})
	}
}

func TestMixedAutomaticAndHumanRequestsStayRedUntilHumanResolution(t *testing.T) {
	observer, key := newWaitObserver(t, 20*time.Millisecond, nil)
	applyWaitNote(t, observer, rpcNotification{Method: "thread/settings/updated", Params: mustJSON(t, map[string]any{
		"threadId": "root", "threadSettings": map[string]any{"approvalsReviewer": "auto_review"},
	})})
	applyWaitNote(t, observer, approvalWaitStatus(t))
	applyWaitNote(t, observer, approvalRequest(t, json.RawMessage(`"auto"`), "auto-item"))
	applyWaitNote(t, observer, autoReviewNote(t, "item/autoApprovalReview/started", "allow"))
	applyWaitNote(t, observer, rpcNotification{ID: json.RawMessage(`"human-input"`), Method: "item/tool/requestUserInput", Params: mustJSON(t, map[string]any{
		"threadId": "root", "turnId": "turn", "itemId": "question", "isBlocking": true, "autoResolutionMs": nil,
	})})
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)
	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`"auto"`)))
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)
	applyWaitNote(t, observer, resolvedRequest(t, json.RawMessage(`"human-input"`)))
	assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
}

func TestAutoReviewCompletionCanBeFollowedByGenuineUserApproval(t *testing.T) {
	observer, key := newWaitObserver(t, 20*time.Millisecond, nil)
	applyWaitNote(t, observer, autoReviewNote(t, "item/autoApprovalReview/started", "deny"))
	applyWaitNote(t, observer, autoReviewNote(t, "item/autoApprovalReview/completed", "deny"))
	applyWaitNote(t, observer, rpcNotification{Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
		"threadId": "root", "status": map[string]any{"type": "active", "activeFlags": []string{}},
	})})
	applyWaitNote(t, observer, rpcNotification{Method: "thread/settings/updated", Params: mustJSON(t, map[string]any{
		"threadId": "root", "threadSettings": map[string]any{"approvalsReviewer": "user"},
	})})
	applyWaitNote(t, observer, approvalWaitStatus(t))
	applyWaitNote(t, observer, approvalRequest(t, json.RawMessage(`"human"`), "human-item"))
	assertWaitAttention(t, observer, key, agentgraph.AttentionApproval)
}

func TestReconnectSnapshotUsesGuardianEvidenceAndDoesNotFlashRed(t *testing.T) {
	observer, key := newWaitObserver(t, 20*time.Millisecond, nil)
	guardian := mustJSON(t, map[string]any{"subAgent": map[string]any{"other": "guardian"}})
	snapshot := newGraphState(
		rpcThread{ID: "root", Status: rpcStatus{Type: "active", ActiveFlags: []string{"waitingOnApproval"}}},
		[]rpcThread{{ID: "guardian", ParentThreadID: "root", Source: guardian, Status: rpcStatus{Type: "active"}}},
		32,
	)
	observer.mu.Lock()
	observer.generation = 2
	observer.mu.Unlock()
	observer.installSnapshot(2, key, "root", snapshot)
	time.Sleep(40 * time.Millisecond)
	node := waitNode(t, observer, key)
	if node.Attention != agentgraph.AttentionNone || node.Runtime != agentgraph.RuntimeActive {
		t.Fatalf("reconnect guardian snapshot flashed attention: %#v", node)
	}
}

func TestAuthoritativeSnapshotRemovalClearsOutstandingHumanRequest(t *testing.T) {
	observer, key := newWaitObserver(t, 20*time.Millisecond, nil)
	applyWaitNote(t, observer, rpcNotification{Method: "thread/settings/updated", Params: mustJSON(t, map[string]any{
		"threadId": "root", "threadSettings": map[string]any{"approvalsReviewer": "user"},
	})})
	applyWaitNote(t, observer, approvalWaitStatus(t))
	applyWaitNote(t, observer, approvalRequest(t, json.RawMessage(`"human"`), "item"))
	assertWaitAttention(t, observer, key, agentgraph.AttentionApproval)

	snapshot := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "active"}}, nil, 32)
	observer.installSnapshot(1, key, "root", snapshot)
	node := waitNode(t, observer, key)
	if node.Attention != agentgraph.AttentionNone || node.Runtime != agentgraph.RuntimeActive {
		t.Fatalf("authoritative omission retained request state: %#v", node)
	}
}

func TestCommandFailureAndTurnCompletionDoNotBorrowAttentionRed(t *testing.T) {
	observer, key := newWaitObserver(t, 20*time.Millisecond, nil)
	applyWaitNote(t, observer, rpcNotification{Method: "item/completed", Params: mustJSON(t, map[string]any{
		"threadId": "root", "turnId": "turn", "item": map[string]any{"id": "command", "type": "commandExecution", "status": "failed"},
	})})
	node := waitNode(t, observer, key)
	if node.Runtime != agentgraph.RuntimeActive || node.Attention != agentgraph.AttentionNone {
		t.Fatalf("command failure changed runtime/attention: %#v", node)
	}
	applyWaitNote(t, observer, rpcNotification{ID: json.RawMessage(`"input"`), Method: "item/tool/requestUserInput", Params: mustJSON(t, map[string]any{
		"threadId": "root", "turnId": "turn", "itemId": "item", "isBlocking": true,
	})})
	assertWaitAttention(t, observer, key, agentgraph.AttentionUserInput)
	applyWaitNote(t, observer, rpcNotification{Method: "turn/completed", Params: mustJSON(t, map[string]any{
		"threadId": "root", "turn": map[string]any{"id": "turn", "status": "completed"},
	})})
	assertWaitAttention(t, observer, key, agentgraph.AttentionNone)
}

func newWaitObserver(t *testing.T, delay time.Duration, diagnostic func(WaitClassificationDiagnostic)) (*Observer, provider.RootKey) {
	t.Helper()
	key := provider.RootKey{PID: 9001, StartedAt: time.Unix(9001, 0)}
	state := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "active"}}, nil, 32)
	now := time.Now()
	observation, err := state.observation(now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	observer := &Observer{
		config: Config{Freshness: time.Second, WaitClassification: delay, Now: time.Now, WaitDiagnostic: diagnostic},
		queue:  provider.NewInvalidationQueue(64),
		roots: map[provider.RootKey]*rootRecord{
			key: {threadID: "root", graph: state, observation: observation, generation: 1},
		},
		generation: 1, connected: true, pendingStatuses: make(map[string]rpcStatus),
	}
	t.Cleanup(func() {
		observer.mu.Lock()
		observer.closed = true
		for _, record := range observer.roots {
			if record.graph != nil {
				record.graph.stopClassifications()
			}
			if record.expiry != nil {
				record.expiry.Stop()
			}
		}
		observer.mu.Unlock()
	})
	return observer, key
}

func applyWaitNote(t *testing.T, observer *Observer, note rpcNotification) {
	t.Helper()
	note.Generation = 1
	observer.handleNotification(note)
}

func approvalWaitStatus(t *testing.T) rpcNotification {
	t.Helper()
	return rpcNotification{Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
		"threadId": "root", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}},
	})}
}

func approvalRequest(t *testing.T, id json.RawMessage, itemID string) rpcNotification {
	t.Helper()
	return rpcNotification{ID: id, Method: "item/commandExecution/requestApproval", Params: mustJSON(t, map[string]any{
		"threadId": "root", "turnId": "turn", "itemId": itemID,
	})}
}

func resolvedRequest(t *testing.T, id json.RawMessage) rpcNotification {
	t.Helper()
	return rpcNotification{Method: "serverRequest/resolved", Params: mustJSON(t, map[string]any{
		"threadId": "root", "requestId": id,
	})}
}

func autoReviewNote(t *testing.T, method, outcome string) rpcNotification {
	t.Helper()
	return rpcNotification{Method: method, Params: mustJSON(t, map[string]any{
		"threadId": "root", "turnId": "turn", "reviewId": "review", "targetItemId": "item",
		"review": map[string]any{"status": outcome},
	})}
}

func waitNode(t *testing.T, observer *Observer, key provider.RootKey) agentgraph.Node {
	t.Helper()
	observer.mu.Lock()
	defer observer.mu.Unlock()
	node := findNode(observer.roots[key].observation, "root")
	if node == nil {
		t.Fatal("root node missing")
	}
	return *node
}

func assertWaitAttention(t *testing.T, observer *Observer, key provider.RootKey, want agentgraph.AttentionState) {
	t.Helper()
	if node := waitNode(t, observer, key); node.Attention != want {
		t.Fatalf("attention = %s, want %s (node=%#v)", node.Attention, want, node)
	}
}

func waitForWaitNode(t *testing.T, observer *Observer, key provider.RootKey, timeout time.Duration, accept func(agentgraph.Node) bool) agentgraph.Node {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		node := waitNode(t, observer, key)
		if accept(node) {
			return node
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for classified node")
	return agentgraph.Node{}
}
