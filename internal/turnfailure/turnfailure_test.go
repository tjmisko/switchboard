package turnfailure

import (
	"encoding/json"
	"testing"
)

func TestFromClaudeShouldClassifyWhenTheTextNamesANetworkFailure(t *testing.T) {
	cases := []struct {
		name, errorType, message string
		want                     Kind
		ok                       bool
	}{
		{"dns failure", "server_error", "API Error: Can't reach the API server — check your internet or DNS (EAI_AGAIN)", Unreachable, true},
		{"typographic apostrophe", "server_error", "API Error: Can’t reach the API server", Unreachable, true},
		{"lost mid-response", "server_error", "API Error: Connection lost mid-response. The response above may be incomplete.", Unreachable, true},
		{"unknown type", "unknown", "ECONNRESET", Unreachable, true},
		{"authentication", "authentication_failed", "Invalid API key", Auth, true},
		{"plain 500", "server_error", "API Error: 500 Internal server error", "", false},
		{"safeguards", "invalid_request", "API Error: safeguards flagged this request", "", false},
		{"rate limit", "rate_limit", "network busy, rate limited", "", false},
		{"billing", "billing_error", "network", "", false},
		{"empty", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := FromClaude(c.errorType, c.message)
			if got != c.want || ok != c.ok {
				t.Fatalf("FromClaude(%q, %q) = %q, %v; want %q, %v", c.errorType, c.message, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestFromPiShouldClassifyWhenTheTextNamesANetworkOrAuthFailure(t *testing.T) {
	cases := []struct {
		message string
		want    Kind
		ok      bool
	}{
		{"Connection error.", Unreachable, true},
		{"Network connection lost.", Unreachable, true},
		{"Request timed out.", Unreachable, true},
		{"Upstream idle timeout exceeded", Unreachable, true},
		{"TypeError: fetch failed", Unreachable, true},
		{"401 Unauthorized", Auth, true},
		{"Invalid API key", Auth, true},
		{"Operation aborted", "", false},
		{"This operation was aborted", "", false},
		{"terminated", "", false},
		{"stream ended without finish_reason", "", false},
		{"ENOSPC: no space left on device, write", "", false},
		{"You have hit your ChatGPT usage limit", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		t.Run(c.message, func(t *testing.T) {
			got, ok := FromPi(c.message)
			if got != c.want || ok != c.ok {
				t.Fatalf("FromPi(%q) = %q, %v; want %q, %v", c.message, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestFromCodexShouldClassifyWhenTheTagOrRoutingMessageNamesAFailure(t *testing.T) {
	cases := []struct {
		name, info, message string
		want                Kind
		ok                  bool
	}{
		{"routing failed", `"other"`, "workspace routing discovery failed", Unreachable, true},
		{"routing timed out", `"other"`, "workspace routing discovery timed out", Unreachable, true},
		{"routing unauthorized", `"other"`, "workspace routing discovery unauthorized (401)", Auth, true},
		{"routing with missing tag", ``, "workspace routing discovery failed", Unreachable, true},
		{"routing with null tag", `null`, "workspace routing discovery failed", Unreachable, true},
		{"object tag", `{"http_connection_failed":{"http_status_code":null}}`, "x", Unreachable, true},
		{"stream connection", `{"response_stream_connection_failed":{"http_status_code":502}}`, "", Unreachable, true},
		{"stream disconnected", `"response_stream_disconnected"`, "", Unreachable, true},
		{"too many attempts", `{"response_too_many_failed_attempts":{}}`, "", Unreachable, true},
		{"unauthorized", `"unauthorized"`, "", Auth, true},
		{"overloaded", `"server_overloaded"`, "workspace routing discovery failed", "", false},
		{"usage limit", `"usage_limit_exceeded"`, "", "", false},
		{"rate limit", `{"rate_limit_exceeded":{}}`, "", "", false},
		{"other with other text", `"other"`, "boom", "", false},
		{"malformed", `{"http_conn`, "workspace routing discovery failed", "", false},
		{"multi-key object", `{"a":1,"b":2}`, "", "", false},
		{"empty", ``, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := FromCodex(json.RawMessage(c.info), c.message)
			if got != c.want || ok != c.ok {
				t.Fatalf("FromCodex(%s, %q) = %q, %v; want %q, %v", c.info, c.message, got, ok, c.want, c.ok)
			}
		})
	}
}
