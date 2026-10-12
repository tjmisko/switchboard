package billing

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Identity contains only classifications. Credential values and full URLs are
// never copied into it, so it is safe to send to the daemon and history.
type Identity struct {
	Gateway           string `json:"gateway,omitempty"`
	ExecutionProvider string `json:"execution_provider,omitempty"`
	BillingRoute      string `json:"billing_route,omitempty"`
	RouteEvidence     string `json:"route_evidence,omitempty"`
}

type authStatus struct {
	AuthMethod  string `json:"authMethod"`
	APIProvider string `json:"apiProvider"`
}

func gatewayHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "api.anthropic.com" {
		return ""
	}
	if host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai") {
		return "openrouter"
	}
	// Unknown hosts carry only a bounded hostname, never a path, token, or port.
	if len(host) > 128 || strings.ContainsAny(host, " /\\@") {
		return "custom"
	}
	return host
}

// Classify uses the auth method reported by Claude Code in the hook's own
// environment. A gateway URL alone cannot override an active plan login.
func Classify(statusJSON []byte, baseURL string, hasGatewayToken, hasSetupToken bool) Identity {
	var status authStatus
	if json.Unmarshal(statusJSON, &status) != nil {
		return Identity{BillingRoute: "unknown", RouteEvidence: "probe_failed"}
	}
	gateway := gatewayHost(baseURL)
	switch status.AuthMethod {
	case "claude.ai":
		return Identity{ExecutionProvider: "anthropic", BillingRoute: "subscription", RouteEvidence: "auth_status"}
	case "oauth_token":
		if gateway != "" && hasGatewayToken {
			return Identity{Gateway: gateway, BillingRoute: "gateway", RouteEvidence: "auth_status"}
		}
		if hasSetupToken && !hasGatewayToken {
			return Identity{ExecutionProvider: "anthropic", BillingRoute: "subscription", RouteEvidence: "auth_status"}
		}
	case "api_key", "api_key_helper":
		if gateway != "" {
			return Identity{Gateway: gateway, BillingRoute: "gateway", RouteEvidence: "auth_status_pending_response"}
		}
		return Identity{ExecutionProvider: "anthropic", BillingRoute: "api", RouteEvidence: "auth_status_pending_response"}
	case "third_party":
		provider := map[string]string{"bedrock": "aws-bedrock", "vertex": "google-vertex", "foundry": "azure"}[strings.ToLower(status.APIProvider)]
		if provider != "" {
			return Identity{ExecutionProvider: provider, BillingRoute: "cloud", RouteEvidence: "auth_status"}
		}
	}
	return Identity{BillingRoute: "unknown", RouteEvidence: "ambiguous_auth_status"}
}

// Probe has a short hard deadline because SessionStart must not wait for an
// authentication subprocess. The command receives the hook's environment.
func Probe() Identity {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "auth", "status", "--json").Output()
	if err != nil {
		return Identity{BillingRoute: "unknown", RouteEvidence: "probe_failed"}
	}
	return Classify(out, os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_AUTH_TOKEN") != "", os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "")
}
