package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

func TestStableTokenUsageNotificationRetainsIdentityAndDeduplicates(t *testing.T) {
	observer, key := fixtureObserver(t)
	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: "thread/updated", Params: mustJSON(t, map[string]any{
		"thread": map[string]any{"id": fixtureRoot, "modelProvider": "openai"},
	})})
	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: "thread/settings/updated", Params: mustJSON(t, map[string]any{
		"threadId": fixtureRoot,
		"threadSettings": map[string]any{
			"approvalsReviewer": "user", "model": "gpt-5.6-sol", "modelProvider": "openai",
			"serviceTier": "fast", "effort": "high",
		},
	})})

	note := readTelemetryFixture(t, "token-usage-updated.json")
	changed, unknown, _ := observer.applyNotificationLocked(rpcNotification{
		Generation: 1, Method: note.Method, Params: note.Params,
	})
	if unknown || !reflect.DeepEqual(changed, []provider.RootKey{key}) {
		t.Fatalf("usage apply = changed %#v unknown %v", changed, unknown)
	}

	root := findNode(observer.roots[key].observation, fixtureRoot)
	if root == nil {
		t.Fatal("usage root missing")
	}
	wantTotal := agentgraph.Usage{
		InputTokens: 1000, CachedInputTokens: 600, CacheWriteInputTokens: 100,
		OutputTokens: 200, ReasoningOutputTokens: 80, TotalTokens: 1200, ModelContextWindow: 272000,
	}
	if root.Usage != wantTotal {
		t.Fatalf("root usage = %#v, want %#v", root.Usage, wantTotal)
	}
	wantIdentity := agentgraph.BillingIdentity{
		AgentClient: "codex", ExecutionProvider: "openai", Model: "gpt-5.6-sol",
		ServiceTier: "fast", ReasoningEffort: "high",
	}
	if root.Billing != wantIdentity {
		t.Fatalf("root billing = %#v, want %#v", root.Billing, wantIdentity)
	}

	first := receiveUsageUpdate(t, observer.UsageUpdates())
	wantDelta := agentgraph.Usage{
		InputTokens: 100, CachedInputTokens: 60, CacheWriteInputTokens: 10,
		OutputTokens: 20, ReasoningOutputTokens: 8, TotalTokens: 120, ModelContextWindow: 272000,
	}
	if first.RootKey != key || first.ThreadID != fixtureRoot || first.TurnID != "turn-usage-1" ||
		first.Delta != wantDelta || first.Total != wantTotal || first.UpdateID == "" || first.Identity != wantIdentity {
		t.Fatalf("first usage update = %#v", first)
	}

	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: note.Method, Params: note.Params})
	assertNoUsageUpdate(t, observer.UsageUpdates())

	// Equal per-turn values are distinct when their provider turn IDs differ and
	// cumulative totals advance. An exact replay of the second update is not.
	secondTotal := addBreakdownForTest(wantTotal, wantDelta)
	secondParams := tokenUsageParamsForTest(fixtureRoot, "turn-usage-2", &wantDelta, &secondTotal)
	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: "thread/tokenUsage/updated", Params: secondParams})
	second := receiveUsageUpdate(t, observer.UsageUpdates())
	if second.TurnID != "turn-usage-2" || second.Delta != wantDelta || second.Total != secondTotal {
		t.Fatalf("second usage update = %#v", second)
	}
	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: "thread/tokenUsage/updated", Params: secondParams})
	assertNoUsageUpdate(t, observer.UsageUpdates())

	// Older/partial app-server versions may omit `last`. Only a guarded
	// monotonic difference from `total` is emitted in that case.
	thirdTotal := secondTotal
	thirdTotal.InputTokens += 50
	thirdTotal.CachedInputTokens += 20
	thirdTotal.OutputTokens += 10
	thirdTotal.ReasoningOutputTokens += 4
	thirdTotal.TotalTokens += 60
	observer.applyNotificationLocked(rpcNotification{
		Generation: 1, Method: "thread/tokenUsage/updated",
		Params: tokenUsageParamsForTest(fixtureRoot, "turn-usage-3", nil, &thirdTotal),
	})
	third := receiveUsageUpdate(t, observer.UsageUpdates())
	if third.Delta.InputTokens != 50 || third.Delta.CachedInputTokens != 20 || third.Delta.OutputTokens != 10 ||
		third.Delta.ReasoningOutputTokens != 4 || third.Delta.TotalTokens != 60 {
		t.Fatalf("derived total delta = %#v", third.Delta)
	}
	observer.applyNotificationLocked(rpcNotification{
		Generation: 1, Method: "thread/tokenUsage/updated",
		Params: tokenUsageParamsForTest(fixtureRoot, "turn-usage-4", nil, &thirdTotal),
	})
	assertNoUsageUpdate(t, observer.UsageUpdates())
}

func TestTokenUsageBeforeThreadMetadataIsRetainedAndReplayed(t *testing.T) {
	observer, key := fixtureObserver(t)
	delta := agentgraph.Usage{InputTokens: 5, OutputTokens: 2, TotalTokens: 7, ModelContextWindow: 100}
	params := tokenUsageParamsForTest("late-child", "late-turn", &delta, &delta)
	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: "thread/tokenUsage/updated", Params: params})
	if len(observer.pendingUsage["late-child"]) != 1 {
		t.Fatalf("pending usage = %#v", observer.pendingUsage)
	}
	assertNoUsageUpdate(t, observer.UsageUpdates())

	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: "thread/started", Params: mustJSON(t, map[string]any{
		"thread": map[string]any{"id": "late-child", "parentThreadId": fixtureRoot, "modelProvider": "openai"},
	})})
	update := receiveUsageUpdate(t, observer.UsageUpdates())
	if update.ThreadID != "late-child" || update.TurnID != "late-turn" || update.Delta != delta {
		t.Fatalf("replayed usage update = %#v", update)
	}
	child := findNode(observer.roots[key].observation, "late-child")
	if child == nil || child.Usage != delta || child.Billing.ExecutionProvider != "openai" {
		t.Fatalf("late child = %#v", child)
	}
	if len(observer.pendingUsage) != 0 {
		t.Fatalf("replayed pending usage retained = %#v", observer.pendingUsage)
	}
}

func TestThreadAndCollaborationMetadataSurviveSnapshots(t *testing.T) {
	state := newGraphState(rpcThread{ID: "root", ModelProvider: "openai", Status: rpcStatus{Type: "idle"}}, nil, 32)
	state.applyCollaboration("turn", rpcItem{
		Type: "collabAgentToolCall", SenderThreadID: "root", ReceiverThreadIDs: []string{"child"},
		Model: "gpt-5.6-luna", ReasoningEffort: "medium",
	})
	if !state.applyThreadSettings("root", rpcThreadSettings{
		Model: "gpt-5.6-sol", ModelProvider: "openai", ServiceTier: "fast", Effort: "high",
	}) {
		t.Fatal("thread settings did not update identity")
	}
	state.nodes["root"].node.Usage = agentgraph.Usage{InputTokens: 10, TotalTokens: 10}

	replacement := newGraphState(rpcThread{ID: "root", ModelProvider: "openai", Status: rpcStatus{Type: "idle"}}, []rpcThread{
		{ID: "child", ParentThreadID: "root", ModelProvider: "openai"},
	}, 32)
	replacement.mergeTelemetryFrom(state)
	root := replacement.nodes["root"].node
	child := replacement.nodes["child"].node
	if root.Billing.Model != "gpt-5.6-sol" || root.Billing.ServiceTier != "fast" || root.Billing.ReasoningEffort != "high" ||
		root.Usage.InputTokens != 10 {
		t.Fatalf("merged root telemetry = %#v", root)
	}
	if child.Billing.Model != "gpt-5.6-luna" || child.Billing.ReasoningEffort != "medium" {
		t.Fatalf("collaboration metadata = %#v", child.Billing)
	}
}

func TestAccountRoutesAndNullableVendorEstimateRemainDistinct(t *testing.T) {
	for _, test := range []struct {
		accountType string
		planType    string
		kind        string
		route       string
		provider    string
	}{
		{"apiKey", "", "api_key", "api", ""},
		{"chatgpt", "pro", "chatgpt", "chatgpt_subscription", ""},
		{"chatgpt", "self_serve_business_usage_based", "chatgpt", "credits", ""},
		{"amazonBedrock", "", "amazon_bedrock", "cloud", "aws-bedrock"},
	} {
		got := accountMetadataFromAccount(rpcAccount{Type: test.accountType, PlanType: test.planType})
		if !got.known || got.accountKind != test.kind || got.billingRoute != test.route || got.executionProvider != test.provider {
			t.Errorf("account %q metadata = %#v", test.accountType, got)
		}
	}

	var decoded accountUsageReadResult
	if err := json.Unmarshal([]byte(`{
		"summary": {},
		"threadUsage": {
			"threadId": "01890f00-0000-7000-8000-000000000001",
			"estimatedUsageCreditsMicros": 420000,
			"estimatedUsageUsdMicros": null,
			"groups": [{
				"model": "gpt-5.6-sol", "reasoningEffort": "high", "speed": "fast",
				"inputTokens": 1000, "cachedInputTokens": 600, "netNewInputTokens": 400,
				"outputTokens": 200, "totalTokens": 1200,
				"estimatedUsageCreditsMicros": 420000
			}]
		}
	}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ThreadUsage == nil || decoded.ThreadUsage.EstimatedUsageUSDMicros != nil {
		t.Fatalf("nullable USD estimate = %#v", decoded.ThreadUsage)
	}

	observer, key := fixtureObserver(t)
	observer.account = accountMetadataFromType("chatgpt")
	observer.roots[key].graph.applyAccountMetadata(observer.account)
	observer.installVendorEstimateLocked(key, observer.roots[key], decoded.ThreadUsage, fixtureNow)
	update := receiveUsageUpdate(t, observer.UsageUpdates())
	if update.VendorEstimate == nil || update.VendorEstimate.EstimatedUsageUSDMicros != nil ||
		update.Identity.BillingRoute != "chatgpt_subscription" || update.Identity.AccountKind != "chatgpt" {
		t.Fatalf("vendor update = %#v", update)
	}
	latest, ok := observer.LatestThreadUsage(fixtureRoot)
	if !ok || latest.EstimatedUsageUSDMicros != nil || len(latest.Groups) != 1 {
		t.Fatalf("latest estimate = %#v, %v", latest, ok)
	}
	*latest.Groups[0].Model = "caller-mutation"
	again, _ := observer.LatestThreadUsage(fixtureRoot)
	if again.Groups[0].Model == nil || *again.Groups[0].Model != "gpt-5.6-sol" {
		t.Fatal("LatestThreadUsage returned shared group storage")
	}
}

func TestObserverReadsBoundedVendorEstimate(t *testing.T) {
	proxy := newFakeProxy()
	proxy.root.ModelProvider = "openai"
	proxy.account = &rpcAccount{Type: "chatgpt", PlanType: "pro"}
	proxy.threadUsage = &ThreadUsageEstimate{
		ThreadID: "root", EstimatedUsageCreditsMicros: 12,
		Groups: []ThreadUsageGroup{{EstimatedUsageCreditsMicros: 12}},
	}
	key := provider.RootKey{PID: 44, StartedAt: time.Unix(2, 0)}
	observer := NewObserver(Config{
		Connector: proxy, ResnapshotInterval: time.Hour, RequestTimeout: time.Second,
		UsageRequestTimeout: time.Second, ReconnectMinimum: time.Millisecond,
		ReconnectMaximum: 2 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	defer observer.Close()
	if err := observer.RegisterHookBinding(key, "root"); err != nil {
		t.Fatal(err)
	}
	ref := provider.RootRef{PID: key.PID, StartedAt: key.StartedAt, Provider: agentgraph.ProviderCodex}
	waitCompleteObservation(t, observer, ref, time.Second)
	update := receiveUsageUpdate(t, observer.UsageUpdates())
	if update.VendorEstimate == nil || update.VendorEstimate.EstimatedUsageCreditsMicros != 12 ||
		update.Identity.ExecutionProvider != "openai" || update.Identity.BillingRoute != "chatgpt_subscription" {
		t.Fatalf("bounded vendor read update = %#v", update)
	}
	methods := proxy.Methods()
	assertMethodOrder(t, methods, []string{"initialize", "initialized", "account/read", "thread/read", "thread/turns/list", "thread/list", "account/usage/read"})
}

func TestUsageQueueAndDedupRetentionAreBounded(t *testing.T) {
	observer, _ := fixtureObserver(t)
	observer.usageUpdates = make(chan UsageUpdate, 1)
	observer.config.UsageDedupLimit = 2
	observer.emitUsageUpdateLocked(UsageUpdate{UpdateID: "first"})
	observer.emitUsageUpdateLocked(UsageUpdate{UpdateID: "second"})
	if got := receiveUsageUpdate(t, observer.UsageUpdates()); got.UpdateID != "second" {
		t.Fatalf("bounded queue retained %#v, want latest", got)
	}
	for _, fingerprint := range []string{"a", "b", "c"} {
		observer.seenUsageFingerprintLocked(fingerprint)
	}
	if len(observer.usageFingerprints) != 2 || len(observer.usageFingerprintOrder) != 2 {
		t.Fatalf("bounded dedup = map %d order %d", len(observer.usageFingerprints), len(observer.usageFingerprintOrder))
	}
}

func readTelemetryFixture(t *testing.T, name string) rpcEnvelope {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "captures", name))
	if err != nil {
		t.Fatal(err)
	}
	var envelope rpcEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func tokenUsageParamsForTest(threadID, turnID string, last, total *agentgraph.Usage) json.RawMessage {
	contextWindow := int64(0)
	if total != nil {
		contextWindow = total.ModelContextWindow
	} else if last != nil {
		contextWindow = last.ModelContextWindow
	}
	usage := map[string]any{"modelContextWindow": contextWindow}
	if last != nil {
		usage["last"] = breakdownForTest(*last)
	}
	if total != nil {
		usage["total"] = breakdownForTest(*total)
	}
	body, _ := json.Marshal(map[string]any{"threadId": threadID, "turnId": turnID, "tokenUsage": usage})
	return body
}

func breakdownForTest(usage agentgraph.Usage) map[string]int64 {
	return map[string]int64{
		"inputTokens": usage.InputTokens, "cachedInputTokens": usage.CachedInputTokens,
		"cacheWriteInputTokens": usage.CacheWriteInputTokens, "outputTokens": usage.OutputTokens,
		"reasoningOutputTokens": usage.ReasoningOutputTokens, "totalTokens": usage.TotalTokens,
	}
}

func addBreakdownForTest(left, right agentgraph.Usage) agentgraph.Usage {
	result, ok := addUsage(left, right)
	if !ok {
		panic("synthetic usage overflow")
	}
	return result
}

func receiveUsageUpdate(t *testing.T, updates <-chan UsageUpdate) UsageUpdate {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage update")
		return UsageUpdate{}
	}
}

func assertNoUsageUpdate(t *testing.T, updates <-chan UsageUpdate) {
	t.Helper()
	select {
	case update := <-updates:
		t.Fatalf("unexpected usage update %#v", update)
	default:
	}
}
