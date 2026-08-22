package state

import (
	"strings"
	"time"
)

// CurrentSchemaVersion is a clean break from the process-bound Codex state
// model. Load ignores incompatible versioned mirrors and unversioned mirrors
// containing Codex; unversioned Claude-only state remains compatible.
const CurrentSchemaVersion = 2

// NameOrigin records who owns the current Codex thread name. User names are
// authoritative; generated and fallback names may be replaced by an explicit
// /rename observed through app-server.
type NameOrigin string

const (
	NameOriginUser      NameOrigin = "user"
	NameOriginGenerated NameOrigin = "generated"
	NameOriginFallback  NameOrigin = "fallback"
)

// AutonameState is content-free operational state exposed for diagnostics.
type AutonameState string

const (
	AutonameNone       AutonameState = ""
	AutonamePending    AutonameState = "pending"
	AutonameGenerated  AutonameState = "generated"
	AutonameFallback   AutonameState = "fallback"
	AutonameSuppressed AutonameState = "suppressed_explicit"
)

// ConversationBinding is the replaceable conversation currently displayed in
// one stable terminal slot. Generation advances whenever ThreadID changes.
type ConversationBinding struct {
	ThreadID   string     `json:"thread_id"`
	Generation uint64     `json:"generation"`
	Name       string     `json:"name,omitempty"`
	NameOrigin NameOrigin `json:"name_origin,omitempty"`
	BoundAt    time.Time  `json:"bound_at,omitzero"`
	ObservedAt time.Time  `json:"observed_at,omitzero"`
}

// RetiredConversation is the identity/name history retained after a /clear.
// Runtime, attention, children, and pending work are never copied here.
type RetiredConversation struct {
	ThreadID   string     `json:"thread_id"`
	Generation uint64     `json:"generation"`
	Name       string     `json:"name,omitempty"`
	NameOrigin NameOrigin `json:"name_origin,omitempty"`
	RetiredAt  time.Time  `json:"retired_at"`
}

// CodexSlot is the stable lifetime of one visible Codex TUI. Endpoint belongs
// to that TUI only; PID/start are liveness and discovery metadata.
type CodexSlot struct {
	SlotID    string    `json:"slot_id"`
	Endpoint  string    `json:"endpoint"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`

	Conversation *ConversationBinding  `json:"conversation,omitempty"`
	Retired      []RetiredConversation `json:"retired,omitempty"`

	EndpointConnected bool          `json:"endpoint_connected"`
	SnapshotAt        time.Time     `json:"snapshot_at,omitzero"`
	Diagnostic        string        `json:"diagnostic,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
	Autoname          AutonameState `json:"autoname,omitempty"`
	// PendingName is in-memory only and lets the observer distinguish its own
	// acknowledged thread/name/set notification from an explicit /rename.
	PendingName       string     `json:"-"`
	PendingNameOrigin NameOrigin `json:"-"`
}

// BindConversation reconciles exact thread identity. It reports rotated for a
// new binding (including the first), and stale when threadID has already been
// retired. Retired identities remain fenced for the slot lifetime, so an
// arbitrarily late observation can never rotate the slot backwards.
func (s *CodexSlot) BindConversation(threadID string, at time.Time) (rotated, stale bool) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return false, false
	}
	if s.Conversation != nil && s.Conversation.ThreadID == threadID {
		return false, false
	}
	if s.Conversation != nil && !at.IsZero() && !s.Conversation.ObservedAt.IsZero() && at.Before(s.Conversation.ObservedAt) {
		return false, true
	}
	for _, old := range s.Retired {
		if old.ThreadID == threadID {
			return false, true
		}
	}
	generation := uint64(1)
	if s.Conversation != nil {
		generation = s.Conversation.Generation + 1
		s.Retired = append(s.Retired, RetiredConversation{
			ThreadID: s.Conversation.ThreadID, Generation: s.Conversation.Generation,
			Name: s.Conversation.Name, NameOrigin: s.Conversation.NameOrigin, RetiredAt: at,
		})
	}
	s.Conversation = &ConversationBinding{ThreadID: threadID, Generation: generation, BoundAt: at, ObservedAt: at}
	s.SnapshotAt = time.Time{}
	s.Diagnostic = "conversation rotated"
	s.Autoname = AutonameNone
	s.PendingName = ""
	s.PendingNameOrigin = ""
	return true, false
}

// MarkConversationObserved advances the slot-level reorder fence after an
// observation has been accepted by the normalized graph reducer.
func (s *CodexSlot) MarkConversationObserved(threadID string, generation uint64, at time.Time) bool {
	if s.Conversation == nil || s.Conversation.ThreadID != threadID || s.Conversation.Generation != generation {
		return false
	}
	if at.Before(s.Conversation.ObservedAt) {
		return false
	}
	s.Conversation.ObservedAt = at
	return true
}

// SetConversationName projects app-server's authoritative thread name onto the
// current binding. Callers must generation/thread-gate before invoking it.
func (s *CodexSlot) SetConversationName(name string, origin NameOrigin) {
	if s.Conversation == nil {
		return
	}
	s.Conversation.Name = strings.TrimSpace(name)
	s.Conversation.NameOrigin = origin
}

func cloneCodexSlot(slot *CodexSlot) *CodexSlot {
	if slot == nil {
		return nil
	}
	cp := *slot
	if slot.Conversation != nil {
		binding := *slot.Conversation
		cp.Conversation = &binding
	}
	cp.Retired = append([]RetiredConversation(nil), slot.Retired...)
	return &cp
}
