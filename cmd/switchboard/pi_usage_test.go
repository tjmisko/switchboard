package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/pricing"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

// piMessage is a Usage hook's payload: one assistant message's counts and
// Pi's own cost for it.
func piMessage(id string, input, output int64, cost float64) func(*rpc.Request) {
	return func(req *rpc.Request) {
		req.Usage = &rpc.HookUsage{
			MessageID: id, Provider: "anthropic", Model: "claude-opus-4-8",
			InputTokens: input, OutputTokens: output, CacheReadTokens: 100, CacheWriteTokens: 10,
			TotalTokens: input + output + 110, CostTotal: &cost,
		}
	}
}

// rootUsage is the bound graph root's usage and billing identity.
func (h *piHarness) rootUsage() (state.AgentUsage, agentgraph.BillingIdentity) {
	h.t.Helper()
	graph := h.session().AgentGraph
	if graph == nil {
		h.t.Fatal("no graph")
	}
	for _, node := range graph.Nodes {
		if node.ID == graph.RootID {
			return node.Usage, node.Billing
		}
	}
	h.t.Fatalf("graph %q has no root node", graph.RootID)
	return state.AgentUsage{}, agentgraph.BillingIdentity{}
}

func usageEvents(events []history.Event) []history.Event {
	var out []history.Event
	for _, event := range events {
		if event.Type == history.EventUsageSample {
			out = append(out, event)
		}
	}
	return out
}

func TestPiUsageShouldAddPisOwnPerMessageCostWithoutRepricing(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"))
	h.hook("UserPromptSubmit", time.Second)
	freshUntil := h.session().AgentGraph.FreshUntil

	h.hook("Usage", 2*time.Second, piMessage("resp_1", 1_000_000, 1000, 0.42))
	h.hook("Usage", 3*time.Second, piMessage("resp_2", 1_000_000, 2000, 0.08))

	usage, billing := h.rootUsage()
	if usage.InputTokens != 2_000_000 || usage.OutputTokens != 3000 || usage.CachedInputTokens != 200 ||
		usage.CacheWriteInputTokens != 20 || usage.TotalTokens != 2_003_220 {
		t.Fatalf("root usage = %+v, want the two messages summed", usage)
	}
	want := agentgraph.BillingIdentity{AgentClient: "pi", ExecutionProvider: "anthropic", Model: "claude-opus-4-8"}
	if billing != want {
		t.Fatalf("billing = %+v, want %+v", billing, want)
	}
	h.wantStatus("usage carries no status", state.StatusWorking)
	if got := h.session().AgentGraph.FreshUntil; !got.Equal(freshUntil) {
		t.Fatalf("usage renewed the hook lease: %v, want %v", got, freshUntil)
	}

	samples := usageEvents(h.events())
	if len(samples) != 2 {
		t.Fatalf("usage samples = %d, want 2", len(samples))
	}
	for _, sample := range samples {
		if sample.Agent != "pi" || sample.SessionID != piTestSession || sample.ExecutionProvider != "anthropic" ||
			sample.Model != "claude-opus-4-8" || sample.UsageEventID == "" {
			t.Fatalf("sample identity = %+v", sample)
		}
	}
	// The catalog prices claude-opus-4-8 input at $5/MTok: a reprice of these
	// 2M input tokens alone would read $10.
	pricingNow := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	totals := history.AggregateTotalsWithCatalogs(samples, pricing.BootstrapCatalogs(), pricingNow)
	if totals.CostUSD == nil || *totals.CostUSD != pricing.USDFromMicros(500_000) {
		t.Fatalf("totals cost = %v, want Pi's own $0.50", totals.CostUSD)
	}
}

func TestPiUsageShouldNotDoubleCountAMessageDeliveredTwice(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"))
	h.hook("Usage", time.Second, piMessage("resp_1", 1000, 100, 0.01))
	h.hook("Usage", 2*time.Second, piMessage("resp_1", 1000, 100, 0.01))
	// Without a message id a hook counts once per message_end: a redelivered
	// hook (same instant, same counts) once, a later message again.
	noID := func(req *rpc.Request) { req.Usage.MessageID = "" }
	h.hook("Usage", 3*time.Second, piMessage("", 500, 50, 0.005), noID)
	h.hook("Usage", 3*time.Second, piMessage("", 500, 50, 0.005), noID)
	h.hook("Usage", 4*time.Second, piMessage("", 500, 50, 0.005), noID)

	usage, _ := h.rootUsage()
	if usage.InputTokens != 2000 || usage.OutputTokens != 200 {
		t.Fatalf("root usage = %+v, want resp_1 once and two id-less messages", usage)
	}
	samples := usageEvents(h.events())
	if len(samples) != 3 {
		t.Fatalf("usage samples = %d, want 3", len(samples))
	}
	totals := history.AggregateTotals(samples)
	if totals.TokIn != 2000 {
		t.Fatalf("history input = %d, want 2000", totals.TokIn)
	}
}

func TestPiUsageShouldResetUsageOnRotation(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"))
	h.hook("Usage", time.Second, piMessage("resp_1", 1000, 100, 0.01))

	h.hook("SessionStart", 2*time.Second, source("new"), session(piTestNext))
	if usage, billing := h.rootUsage(); !usage.IsZero() || !billing.IsZero() {
		t.Fatalf("after /new the root kept the old session's usage %+v billing %+v", usage, billing)
	}
	// The same message id in the new session is a different message.
	h.hook("Usage", 3*time.Second, session(piTestNext), piMessage("resp_1", 300, 30, 0.003))
	if usage, _ := h.rootUsage(); usage.InputTokens != 300 {
		t.Fatalf("new session usage = %+v, want only its own message", usage)
	}

	bySession := map[string]int64{}
	for _, sample := range usageEvents(h.events()) {
		bySession[sample.SessionID] += sample.Usage.InputTokens
	}
	if bySession[piTestSession] != 1000 || bySession[piTestNext] != 300 {
		t.Fatalf("history usage by session = %v, want each session's own", bySession)
	}
}

func TestPiUsageShouldKeepItsTotalsWhenAStatusHookLands(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"))
	h.hook("Usage", time.Second, piMessage("resp_1", 1000, 100, 0.01))
	h.hook("Stop", 2*time.Second)
	if usage, billing := h.rootUsage(); usage.InputTokens != 1000 || billing.AgentClient != "pi" {
		t.Fatalf("a status hook dropped the usage: %+v %+v", usage, billing)
	}
}
