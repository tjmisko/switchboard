package state

import "strings"

// CurrentSchemaVersion is a clean break from the prior Codex identity model.
// Incompatible mirrors are ignored and live sessions are rebuilt by discovery
// and hooks.
const CurrentSchemaVersion = 3

// DisplayNameOrigin records where a display name came from. Native Codex names
// are not copied into this record; they remain graph metadata and take
// precedence after a later authoritative rename.
type DisplayNameOrigin string

const (
	DisplayNameGenerated DisplayNameOrigin = "generated"
	DisplayNameFallback  DisplayNameOrigin = "fallback"
	// DisplayNameNative is a name the user set in the agent itself: Pi's
	// /name, which its extension forwards. Pi has no adapter graph to carry
	// it the way Codex's app-server carries its own, so it lives here, bound
	// to the Pi session it was set in.
	DisplayNameNative DisplayNameOrigin = "native"
)

// DisplayName is Switchboard-owned display metadata for one Codex
// conversation, or Pi's own name for one Pi session. NativeBaseline is nil until a complete app-server observation
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

// NativeFor reports whether the record is an agent's own name (Pi's /name)
// bound to sessionID. It is separate from ValidFor, which keeps refusing a
// native record: a Codex native name is never copied into this record.
func (n *DisplayName) NativeFor(sessionID string) bool {
	if n == nil || n.Origin != DisplayNameNative || strings.TrimSpace(n.Value) == "" || strings.TrimSpace(n.ConversationID) == "" {
		return false
	}
	return n.ConversationID == strings.TrimSpace(sessionID)
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
