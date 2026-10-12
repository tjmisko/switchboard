package history

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestCarrySessionIdentitiesAcrossMidnight(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 8, 25, 23, 0, 0, 0, time.Local)
	identity := Event{Ts: start, Type: EventSessionIdentity, Agent: "claude", SessionID: "same", ExecutionProvider: "anthropic", BillingRoute: "api", RouteEvidence: "auth_status_pending_response"}
	present := false
	evidence := Event{Ts: start.Add(time.Minute), Type: EventSessionIdentity, Agent: "claude", SessionID: "same", RateLimitsPresent: &present}
	f, err := os.Create(DayPath(dir, "2026-08-25"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []Event{identity, evidence} {
		if err := json.NewEncoder(f).Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 8, 26, 0, 0, 0, 0, time.Local)
	usage := Event{Ts: from.Add(time.Minute), Type: EventUsageSample, Agent: "claude", SessionID: "same"}
	carried, err := CarrySessionIdentities(dir, from, []Event{usage})
	if err != nil {
		t.Fatal(err)
	}
	joined := JoinSessionIdentities(append(carried, usage))
	if len(carried) != 2 || joined[len(joined)-1].BillingRoute != "api" || joined[len(joined)-1].RouteEvidence != "rate_limits_absent" {
		t.Fatalf("carried = %+v; joined usage = %+v", carried, joined[len(joined)-1])
	}
}
