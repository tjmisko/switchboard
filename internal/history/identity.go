package history

type sessionRoute struct {
	base     Event
	seenBase bool
	evidence *bool
}

// joinSessionIdentities resolves each session's latest auth classification and
// its response evidence before any usage is priced. A later /login correction
// applies to the whole session. Explicit request-level routes from Pi/Codex
// remain authoritative.
func JoinSessionIdentities(events []Event) []Event {
	routes := make(map[string]sessionRoute)
	for _, ev := range events {
		if ev.Type != EventSessionIdentity || ev.SessionID == "" {
			continue
		}
		key := ev.Agent + "\x00" + ev.SessionID
		route := routes[key]
		if ev.RateLimitsPresent != nil {
			present := *ev.RateLimitsPresent
			route.evidence = &present
		} else if !route.seenBase || !ev.Ts.Before(route.base.Ts) {
			route.base, route.seenBase = ev, true
		}
		routes[key] = route
	}
	resolved := make(map[string]Event, len(routes))
	for key, route := range routes {
		identity := route.base
		if identity.RouteEvidence == "auth_status_pending_response" && identity.BillingRoute == "api" {
			identity.BillingRoute = ""
		}
		if route.evidence != nil && identity.Gateway == "" &&
			(identity.ExecutionProvider == "anthropic" || identity.BillingRoute == "subscription" || identity.BillingRoute == "api") {
			if *route.evidence {
				identity.ExecutionProvider, identity.BillingRoute = "anthropic", "subscription"
				identity.RouteEvidence = "rate_limits_present"
			} else {
				identity.ExecutionProvider, identity.BillingRoute = "anthropic", "api"
				identity.RouteEvidence = "rate_limits_absent"
			}
		}
		resolved[key] = identity
	}
	joined := append([]Event(nil), events...)
	for i := range joined {
		ev := &joined[i]
		identity, exists := resolved[ev.Agent+"\x00"+ev.SessionID]
		if !exists {
			continue
		}
		if ev.Type == EventSessionIdentity {
			ev.Gateway, ev.ExecutionProvider = identity.Gateway, identity.ExecutionProvider
			ev.BillingRoute, ev.RouteEvidence = identity.BillingRoute, identity.RouteEvidence
			continue
		}
		if ev.Type != EventUsageSample && ev.Type != EventUsageCutover {
			continue
		}
		if ev.BillingRoute == "" {
			if identity.BillingRoute != "unknown" {
				ev.BillingRoute = identity.BillingRoute
				ev.RouteEvidence = identity.RouteEvidence
			}
			if ev.ExecutionProvider == "" {
				ev.ExecutionProvider = identity.ExecutionProvider
			}
			if ev.Gateway == "" {
				ev.Gateway = identity.Gateway
			}
		}
	}
	return joined
}
