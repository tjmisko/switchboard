package history

import (
	"testing"
	"time"
)

func TestSessionIdentityJoinsWithoutChangingExplicitProviderUsage(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Ts: now, Type: EventUsageSample, Agent: "claude", SessionID: "a", Model: "claude-opus-5-5"},
		{Ts: now.Add(time.Second), Type: EventSessionIdentity, Agent: "claude", SessionID: "a", ExecutionProvider: "anthropic", BillingRoute: "subscription", RouteEvidence: "auth_status"},
		{Ts: now.Add(2 * time.Second), Type: EventUsageSample, Agent: "pi", SessionID: "a", Gateway: "openrouter", ExecutionProvider: "", BillingRoute: "gateway"},
	}
	got := JoinSessionIdentities(events)
	if got[0].BillingRoute != "subscription" || got[0].ExecutionProvider != "anthropic" {
		t.Fatalf("Claude usage not joined: %+v", got[0])
	}
	if got[2].BillingRoute != "gateway" || got[2].ExecutionProvider != "" {
		t.Fatalf("Pi gateway was overwritten or inferred: %+v", got[2])
	}
	if events[0].BillingRoute != "" {
		t.Fatal("join mutated raw history")
	}
}

func TestPendingAPIKeyDoesNotBecomeBilledWithoutResponseEvidence(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Ts: now, Type: EventSessionIdentity, Agent: "claude", SessionID: "a", ExecutionProvider: "anthropic", BillingRoute: "api", RouteEvidence: "auth_status_pending_response"},
		{Ts: now, Type: EventUsageSample, Agent: "claude", SessionID: "a"},
	}
	if got := JoinSessionIdentities(events)[1]; got.BillingRoute != "" {
		t.Fatalf("unconfirmed API key classified as billed: %+v", got)
	}
}

func TestRateLimitResponseEvidenceSettlesAnAmbiguousAPIKey(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		present bool
		base    string
		want    string
	}{{true, "api", "subscription"}, {false, "api", "api"}, {false, "subscription", "api"}} {
		evidence := "auth_status_pending_response"
		if tc.base == "subscription" {
			evidence = "auth_status"
		}
		events := []Event{
			{Ts: now, Type: EventSessionIdentity, Agent: "claude", SessionID: "a", ExecutionProvider: "anthropic", BillingRoute: tc.base, RouteEvidence: evidence},
			{Ts: now.Add(time.Second), Type: EventUsageSample, Agent: "claude", SessionID: "a"},
			{Ts: now.Add(2 * time.Second), Type: EventSessionIdentity, Agent: "claude", SessionID: "a", RateLimitsPresent: &tc.present},
		}
		got := JoinSessionIdentities(events)
		if got[1].BillingRoute != tc.want || got[2].BillingRoute != tc.want {
			t.Fatalf("present=%v usage=%+v evidence=%+v", tc.present, got[1], got[2])
		}
	}
}

func TestUnknownRouteRemainsUnknownWithoutSessionEvidence(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Ts: now, Type: EventUsageSample, Agent: "claude", SessionID: "old"},
		{Ts: now, Type: EventSessionIdentity, Agent: "claude", SessionID: "new", BillingRoute: "api", ExecutionProvider: "anthropic", RouteEvidence: "rate_limits_absent"},
		{Ts: now.Add(time.Second), Type: EventUsageSample, Agent: "claude", SessionID: "new"},
	}
	got := JoinSessionIdentities(events)
	if got[0].BillingRoute != "" || got[0].RouteEvidence != "" || got[0].ExecutionProvider != "" {
		t.Fatalf("unknown route changed: %+v", got[0])
	}
	if got[2].BillingRoute != "api" || got[2].RouteEvidence != "rate_limits_absent" {
		t.Fatalf("detected route lost: %+v", got[2])
	}
}

func TestExplicitPiGatewayUsageKeepsUnknownUpstream(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Ts: now, Type: EventUsageSample, Agent: "pi", SessionID: "same", Gateway: "openrouter", BillingRoute: "gateway"},
		{Ts: now.Add(time.Second), Type: EventSessionIdentity, Agent: "pi", SessionID: "same", Gateway: "openrouter", ExecutionProvider: "deepinfra", BillingRoute: "gateway"},
		{Ts: now.Add(2 * time.Second), Type: EventUsageSample, Agent: "pi", SessionID: "same", Gateway: "openrouter", ExecutionProvider: "deepinfra", BillingRoute: "gateway"},
	}
	joined := JoinSessionIdentities(events)
	if joined[0].ExecutionProvider != "" || joined[2].ExecutionProvider != "deepinfra" {
		t.Fatalf("per-response upstreams changed: %+v", joined)
	}
}

func TestEarlierResponseEvidenceDoesNotSettleNewAuthProbe(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	present := true
	events := []Event{
		{Ts: now, Type: EventSessionIdentity, Agent: "claude", SessionID: "same", BillingRoute: "subscription", ExecutionProvider: "anthropic"},
		{Ts: now.Add(time.Second), Type: EventSessionIdentity, Agent: "claude", SessionID: "same", RateLimitsPresent: &present},
		{Ts: now.Add(2 * time.Second), Type: EventSessionIdentity, Agent: "claude", SessionID: "same", BillingRoute: "api", ExecutionProvider: "anthropic", RouteEvidence: "auth_status_pending_response"},
		{Ts: now.Add(3 * time.Second), Type: EventUsageSample, Agent: "claude", SessionID: "same"},
	}
	got := JoinSessionIdentities(events)
	if got[3].BillingRoute != "" {
		t.Fatalf("prior response settled new auth: %+v", got[3])
	}
}
