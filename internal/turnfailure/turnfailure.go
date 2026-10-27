// Package turnfailure classifies provider error text as a turn that ended on
// an error only a human can unblock: the network is unreachable, or the
// credentials are rejected. It runs at the hook privacy boundary
// (switchboard-ctl) and on rollout reads: the message text never leaves it,
// only the verdict.
//
// See docs/network-error-status-plan.md for provenance of the forms below.
package turnfailure

import (
	"encoding/json"
	"strings"
)

// Kind is what blocked the turn.
type Kind string

const (
	// Unreachable means the provider could not be reached (network, DNS, timeout).
	Unreachable Kind = "unreachable"
	// Auth means the provider rejected the credentials.
	Auth Kind = "auth"
)

var claudeUnreachableMarkers = []string{
	"can't reach the api server",
	"connection lost mid-response",
	"connection error",
	"eai_again",
	"enotfound",
	"econnrefused",
	"econnreset",
	"etimedout",
	"network",
}

var piUnreachableMarkers = []string{
	"connection error",
	"network connection lost",
	"fetch failed",
	"request timed out",
	"upstream idle timeout",
	"eai_again",
	"enotfound",
	"econnrefused",
	"econnreset",
	"etimedout",
}

var piAuthMarkers = []string{"401", "invalid api key", "unauthorized"}

var codexUnreachableTags = map[string]bool{
	"http_connection_failed":            true,
	"response_stream_connection_failed": true,
	"response_stream_disconnected":      true,
	"response_too_many_failed_attempts": true,
}

// FromClaude classifies a Claude Code StopFailure. errorType is the hook's
// `error` field and message its `last_assistant_message`. Only
// server_error/unknown text naming a network failure is Unreachable; plain 5xx,
// rate limits, billing and content-policy stops carry no verdict.
//
//	API Error: Can't reach the API server — check your internet or DNS (EAI_AGAIN)
//	API Error: Connection lost mid-response. The response above may be incomplete.
func FromClaude(errorType, message string) (Kind, bool) {
	switch errorType {
	case "authentication_failed":
		return Auth, true
	case "server_error", "unknown":
		if containsAny(normalize(message), claudeUnreachableMarkers) {
			return Unreachable, true
		}
	}
	return "", false
}

// FromPi classifies the errorMessage of a Pi run that ended with stopReason
// "error". Aborts, terminations and local disk errors carry no verdict.
//
//	Connection error.
//	Network connection lost.
//	Request timed out.
func FromPi(message string) (Kind, bool) {
	text := normalize(message)
	if containsAny(text, piUnreachableMarkers) {
		return Unreachable, true
	}
	if containsAny(text, piAuthMarkers) {
		return Auth, true
	}
	return "", false
}

// FromCodex classifies a Codex task_complete error. errorInfo is the raw
// codex_error_info: a JSON string tag or a single-key object keyed by the tag.
// message is error.message, consulted only when the tag is "other" or absent.
func FromCodex(errorInfo json.RawMessage, message string) (Kind, bool) {
	tag, ok := codexTag(errorInfo)
	if !ok {
		return "", false
	}
	if codexUnreachableTags[tag] {
		return Unreachable, true
	}
	if tag == "unauthorized" {
		return Auth, true
	}
	if tag != "other" && tag != "" {
		return "", false
	}
	switch normalize(message) {
	case "workspace routing discovery failed", "workspace routing discovery timed out":
		return Unreachable, true
	case "workspace routing discovery unauthorized (401)":
		return Auth, true
	}
	return "", false
}

// codexTag extracts the tag; ok is false for malformed JSON. A missing or null
// value yields an empty tag.
func codexTag(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", true
	}
	var tag string
	if json.Unmarshal(raw, &tag) == nil {
		return tag, true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return "", false
	}
	if len(object) != 1 {
		return "", len(object) == 0
	}
	for key := range object {
		return key, true
	}
	return "", false
}

func normalize(text string) string {
	text = strings.ReplaceAll(text, "’", "'")
	return strings.ToLower(strings.TrimSpace(text))
}

func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
