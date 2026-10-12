package main

import "testing"

func TestStatuslineBillingEvidenceWaitsForResponseAndSendsOnlyPresence(t *testing.T) {
	for _, tc := range []struct {
		body string
		want *bool
	}{
		{`{"session_id":"a","rate_limits":{"five_hour":{}}}`, nil},
		{`{"session_id":"a","context_window":{"current_usage":{"input_tokens":0}},"rate_limits":{"five_hour":{}}}`, boolPtr(true)},
		{`{"session_id":"a","context_window":{"total_input_tokens":50}}`, boolPtr(false)},
		{`{"session_id":"a","cost":{"total_cost_usd":0.01}}`, boolPtr(false)},
	} {
		id, got := statuslineEvidence([]byte(tc.body))
		if tc.want == nil {
			if id != "" || got != nil {
				t.Fatalf("premature evidence: id=%q flag=%v", id, got)
			}
			continue
		}
		if id != "a" || got == nil || *got != *tc.want {
			t.Fatalf("id=%q flag=%v", id, got)
		}
	}
}

func boolPtr(value bool) *bool { return &value }
