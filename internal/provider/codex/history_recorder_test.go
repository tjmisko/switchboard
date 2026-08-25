package codex

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
)

func TestHistoryRecorderWritesVersionedUsageAndNullableVendorSnapshot(t *testing.T) {
	dir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailMinimal, Dir: dir})
	recorder := NewHistoryUsageRecorder(sink)
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.Local)
	update := UsageUpdate{
		RootKey: provider.RootKey{PID: 22, StartedAt: time.Unix(1, 0)}, RootSessionID: "root",
		ThreadID: "child", ParentThreadID: "root", TurnID: "turn", UpdateID: "usage-id", Revision: 2,
		Source:   agentgraph.SourceCodexRollout,
		Identity: agentgraph.BillingIdentity{AgentClient: "codex", ExecutionProvider: "openai", AuthMode: "api_key", BillingRoute: "api", Model: "gpt-5.6-sol"},
		Delta:    agentgraph.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
		Total:    agentgraph.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}, ObservedAt: at,
	}
	if err := recorder.PersistUsage(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	update.UpdateID, update.Revision = "vendor-id", 3
	update.VendorEstimate = &ThreadUsageEstimate{
		ThreadID: "child", EstimatedUsageCreditsMicros: 42, EstimatedUsageUSDMicros: nil,
		ObservedAt: at, Stale: true,
	}
	if err := recorder.PersistVendorUsage(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	sink.Close()
	events, err := history.ReadRange(dir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].SchemaVersion != history.HistorySchemaVersion || events[0].Usage == nil ||
		events[0].Usage.InputTokens != 10 || events[0].UsageTotal == nil || events[0].UsageTotal.InputTokens != 100 ||
		events[0].AuthMode != "api_key" || events[0].BillingRoute != "api" || events[0].UsageCoverage != "tokens_only" {
		t.Fatalf("usage history event = %#v", events)
	}
	if events[1].SchemaVersion != history.HistorySchemaVersion || events[1].Type != history.EventVendorUsageSnapshot ||
		events[1].VendorUsage == nil || events[1].VendorUsage.EstimatedUsageUSD != nil || !events[1].VendorUsage.Stale {
		t.Fatalf("vendor history event = %#v", events[1])
	}
}

func TestHistoryRecorderMarksNonAPIRoutesTokenOnly(t *testing.T) {
	dir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailMinimal, Dir: dir})
	at := time.Date(2026, 8, 25, 12, 30, 0, 0, time.Local)
	update := UsageUpdate{
		RootKey: provider.RootKey{PID: 24, StartedAt: time.Unix(1, 0)}, RootSessionID: "root",
		ThreadID: "root", TurnID: "turn", UpdateID: "usage-non-api", Revision: 1,
		Source: agentgraph.SourceCodexRollout,
		Identity: agentgraph.BillingIdentity{
			AgentClient: "codex", ExecutionProvider: "openai", AuthMode: "chatgpt", Model: "gpt-5.6-sol",
		},
		Delta: agentgraph.Usage{InputTokens: 10, TotalTokens: 10},
		Total: agentgraph.Usage{InputTokens: 10, TotalTokens: 10}, ObservedAt: at,
	}
	if err := NewHistoryUsageRecorder(sink).PersistUsage(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	sink.Close()
	events, err := history.ReadRange(dir, time.Time{}, time.Time{})
	if err != nil || len(events) != 1 || events[0].UsageCoverage != "tokens_only" {
		t.Fatalf("non-API usage coverage = %#v, %v", events, err)
	}
}

func TestVendorRevisionAfterRecorderRestartSupersedesDurableSnapshot(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 8, 25, 13, 0, 0, 100, time.Local)
	update := UsageUpdate{
		RootKey: provider.RootKey{PID: 23, StartedAt: time.Unix(1, 0)}, RootSessionID: "root", ThreadID: "root",
		UpdateID: vendorSnapshotID("root"), Revision: nextVendorRevision(0, at), Source: agentgraph.SourceCodexAppServer,
		VendorEstimate: &ThreadUsageEstimate{ThreadID: "root", EstimatedUsageCreditsMicros: 1, ObservedAt: at}, ObservedAt: at,
	}
	first := history.NewSink(history.Config{Enabled: true, Detail: history.DetailMinimal, Dir: dir})
	if err := NewHistoryUsageRecorder(first).PersistVendorUsage(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	first.Close()

	// A fresh observer starts with an in-memory prior of zero. The timestamp
	// seed must still exceed the durable prior for the same stable snapshot ID.
	update.Revision = nextVendorRevision(0, at.Add(time.Second))
	update.VendorEstimate.EstimatedUsageCreditsMicros = 2
	update.ObservedAt = at.Add(time.Second)
	second := history.NewSink(history.Config{Enabled: true, Detail: history.DetailMinimal, Dir: dir})
	if err := NewHistoryUsageRecorder(second).PersistVendorUsage(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	second.Close()

	events, err := history.ReadRange(dir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].UsageRevision <= events[0].UsageRevision ||
		events[1].VendorUsage == nil || events[1].VendorUsage.EstimatedUsageCredits.Micros() != 2 {
		t.Fatalf("restart vendor snapshots = %#v", events)
	}
}
