package billing

import "testing"

func TestClassifyClaudeRoute(t *testing.T) {
	tests := []struct {
		name, status, base       string
		gatewayToken, setupToken bool
		want                     Identity
	}{
		{"plan despite base URL", `{"authMethod":"claude.ai","apiProvider":"firstParty"}`, "https://openrouter.ai/api", false, false, Identity{ExecutionProvider: "anthropic", BillingRoute: "subscription", RouteEvidence: "auth_status"}},
		{"gateway token", `{"authMethod":"oauth_token","apiProvider":"firstParty"}`, "https://openrouter.ai/api/v1?key=secret", true, false, Identity{Gateway: "openrouter", BillingRoute: "gateway", RouteEvidence: "auth_status"}},
		{"setup token", `{"authMethod":"oauth_token"}`, "", false, true, Identity{ExecutionProvider: "anthropic", BillingRoute: "subscription", RouteEvidence: "auth_status"}},
		{"api key pending", `{"authMethod":"api_key"}`, "", false, false, Identity{ExecutionProvider: "anthropic", BillingRoute: "api", RouteEvidence: "auth_status_pending_response"}},
		{"bedrock", `{"authMethod":"third_party","apiProvider":"bedrock"}`, "", false, false, Identity{ExecutionProvider: "aws-bedrock", BillingRoute: "cloud", RouteEvidence: "auth_status"}},
		{"failed", `{`, "", false, false, Identity{BillingRoute: "unknown", RouteEvidence: "probe_failed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify([]byte(tc.status), tc.base, tc.gatewayToken, tc.setupToken)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
