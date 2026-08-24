package state

import "strings"

// CurrentSchemaVersion is a clean break from the prior Codex identity model.
// Incompatible mirrors are ignored and live sessions are rebuilt by discovery
// and hooks.
const CurrentSchemaVersion = 3

// DisplayNameOrigin records how Switchboard produced a Codex display name.
// Native Codex names are not copied into this record; they remain graph
// metadata and take precedence after a later authoritative rename.
type DisplayNameOrigin string

const (
	DisplayNameGenerated DisplayNameOrigin = "generated"
	DisplayNameFallback  DisplayNameOrigin = "fallback"
)

// DisplayName is Switchboard-owned display metadata for one Codex
// conversation. NativeBaseline is nil until a complete app-server observation
// is available; a non-nil pointer deliberately distinguishes an authoritative
// empty native name from an unavailable observation.
type DisplayName struct {
	Value          string            `json:"value"`
	Origin         DisplayNameOrigin `json:"origin"`
	ConversationID string            `json:"conversation_id"`
	NativeBaseline *string           `json:"native_baseline,omitempty"`
}

// ValidFor reports whether the record is safe to render for conversationID.
// Validation is intentionally strict so malformed persisted records fail
// closed to the ordinary native-name/short-ID fallbacks.
func (n *DisplayName) ValidFor(conversationID string) bool {
	if n == nil || strings.TrimSpace(n.Value) == "" || strings.TrimSpace(n.ConversationID) == "" {
		return false
	}
	if n.Origin != DisplayNameGenerated && n.Origin != DisplayNameFallback {
		return false
	}
	return n.ConversationID == strings.TrimSpace(conversationID)
}

func cloneDisplayName(name *DisplayName) *DisplayName {
	if name == nil {
		return nil
	}
	clone := *name
	if name.NativeBaseline != nil {
		baseline := *name.NativeBaseline
		clone.NativeBaseline = &baseline
	}
	return &clone
}
