package agentgraph

import "time"

// ProviderKind identifies the adapter that produced an observation.
type ProviderKind string

const (
	ProviderUnknown ProviderKind = ""
	ProviderClaude  ProviderKind = "claude"
	ProviderCodex   ProviderKind = "codex"
)

// RuntimeState describes what a thread is doing now. RuntimeUnknown is an
// explicit state; the empty Go value is canonicalized to it by Normalize.
type RuntimeState string

const (
	RuntimeUnknown     RuntimeState = "unknown"
	RuntimeNotLoaded   RuntimeState = "not_loaded"
	RuntimeIdle        RuntimeState = "idle"
	RuntimeActive      RuntimeState = "active"
	RuntimeSystemError RuntimeState = "system_error"
)

// Valid reports whether r is one of the neutral runtime states.
func (r RuntimeState) Valid() bool {
	switch r {
	case RuntimeUnknown, RuntimeNotLoaded, RuntimeIdle, RuntimeActive, RuntimeSystemError:
		return true
	default:
		return false
	}
}

// AttentionState describes whether and why a person must respond. It remains
// independent of runtime and lifecycle.
type AttentionState string

const (
	AttentionNone      AttentionState = "none"
	AttentionApproval  AttentionState = "approval"
	AttentionUserInput AttentionState = "user_input"
)

// Valid reports whether a is one of the neutral attention states.
func (a AttentionState) Valid() bool {
	switch a {
	case AttentionNone, AttentionApproval, AttentionUserInput:
		return true
	default:
		return false
	}
}

// LifecycleState describes orchestration lifecycle, especially for child
// agents. The values cover every collaborative-agent status in the Codex 0.149
// app-server schema after provider-neutral spelling conversion.
type LifecycleState string

const (
	LifecycleUnknown     LifecycleState = "unknown"
	LifecyclePending     LifecycleState = "pending"
	LifecycleRunning     LifecycleState = "running"
	LifecycleCompleted   LifecycleState = "completed"
	LifecycleInterrupted LifecycleState = "interrupted"
	LifecycleErrored     LifecycleState = "errored"
	LifecycleShutdown    LifecycleState = "shutdown"
	LifecycleNotFound    LifecycleState = "not_found"
)

// Valid reports whether l is one of the neutral lifecycle states.
func (l LifecycleState) Valid() bool {
	switch l {
	case LifecycleUnknown, LifecyclePending, LifecycleRunning, LifecycleCompleted,
		LifecycleInterrupted, LifecycleErrored, LifecycleShutdown, LifecycleNotFound:
		return true
	default:
		return false
	}
}

// Terminal reports whether l denotes work that can remain for display/history
// but must not count as live work or waiting attention.
func (l LifecycleState) Terminal() bool {
	switch l {
	case LifecycleCompleted, LifecycleInterrupted, LifecycleErrored, LifecycleShutdown, LifecycleNotFound:
		return true
	default:
		return false
	}
}

// SourceKind records where an observation came from. It is deliberately not a
// confidence score; source fusion and precedence belong to provider adapters
// and daemon orchestration, not the reducer.
type SourceKind string

const (
	SourceUnknown           SourceKind = ""
	SourceCodexAppServer    SourceKind = "codex_app_server"
	SourceHook              SourceKind = "hook"
	SourceClaudeTranscript  SourceKind = "claude_transcript"
	SourceCodexRollout      SourceKind = "codex_rollout"
	SourceRestoredLastKnown SourceKind = "restored_last_known"
)

// Usage is optional token accounting for one node. Its zero value means usage
// was unavailable and never affects status reduction. The fields encompass the
// Codex app-server token breakdown and Claude's current transcript accounting.
type Usage struct {
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    int64
}

// IsZero reports whether no usage was supplied.
func (u Usage) IsZero() bool {
	return u == (Usage{})
}

// BillingIdentity records the non-secret provider dimensions needed to price
// one node's usage. AgentClient identifies the local client implementation;
// ExecutionProvider identifies the model backend and must not be inferred from
// AgentClient. BillingRoute and AccountKind are coarse classifications only --
// adapters must never place account identifiers or credentials in this value.
//
// Model, service tier, speed, and reasoning effort are retained at the node so
// provider adapters can preserve request-level pricing dimensions before a
// history projection aggregates them.
type BillingIdentity struct {
	AgentClient       string `json:"agent_client,omitempty"`
	ExecutionProvider string `json:"execution_provider,omitempty"`
	// AuthMode describes how the client authenticated. It is deliberately
	// separate from BillingRoute: ChatGPT authentication does not prove whether
	// a particular turn consumed an included allowance or purchased credits.
	AuthMode        string `json:"auth_mode,omitempty"`
	BillingRoute    string `json:"billing_route,omitempty"`
	AccountKind     string `json:"account_kind,omitempty"`
	Model           string `json:"model,omitempty"`
	ServiceTier     string `json:"service_tier,omitempty"`
	Speed           string `json:"speed,omitempty"`
	InferenceGeo    string `json:"inference_geo,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// IsZero reports whether no billing identity metadata was supplied.
func (i BillingIdentity) IsZero() bool {
	return i == (BillingIdentity{})
}

// Node is one explicitly identified thread in an observation. ParentID is
// empty only for the root. Nickname, role, description, timestamps, and usage
// are optional and do not affect status.
type Node struct {
	ID          string
	ParentID    string
	Nickname    string
	Role        string
	Description string
	Runtime     RuntimeState
	Attention   AttentionState
	Lifecycle   LifecycleState
	StartedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt time.Time
	Billing     BillingIdentity
	Usage       Usage
}

// Observation is a provider-owned, bounded snapshot of one root and its
// descendants. Complete distinguishes authoritative omission/deletion from a
// partial view. Diagnostic is for in-memory logging only and must never contain
// prompts, commands, tool inputs, or other user content.
type Observation struct {
	Provider   ProviderKind
	RootID     string
	Nodes      []Node
	Source     SourceKind
	ObservedAt time.Time
	FreshUntil time.Time
	Complete   bool
	Diagnostic string
}

// Fresh reports whether now lies within the caller-supplied half-open freshness
// interval. Source and completeness intentionally do not affect freshness.
func (o Observation) Fresh(now time.Time) bool {
	if o.ObservedAt.IsZero() || o.FreshUntil.IsZero() {
		return false
	}
	return !now.Before(o.ObservedAt) && now.Before(o.FreshUntil)
}

// Clone returns a detached value snapshot. Nodes contains no reference-bearing
// fields, so cloning the slice provides full deep-copy semantics for this
// contract version.
func (o Observation) Clone() Observation {
	clone := o
	clone.Nodes = append([]Node(nil), o.Nodes...)
	return clone
}

// Summary is the provider-neutral reduction of a fresh observation. Attention
// chooses approval before user input only for compact display; ApprovalNodes
// and UserInputNodes preserve both kinds and WaitingNodes is their sum.
type Summary struct {
	Runtime        RuntimeState
	Attention      AttentionState
	LegacyStatus   string
	LiveChildren   int
	WaitingNodes   int
	ApprovalNodes  int
	UserInputNodes int
	ErrorNodes     int
	Since          time.Time
}

const (
	LegacyWorking    = "working"
	LegacyIdle       = "idle"
	LegacyPermission = "permission"
	LegacyDelegating = "delegating"
)
