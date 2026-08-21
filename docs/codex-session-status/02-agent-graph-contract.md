# Phase 2 — neutral agent-graph contract

## Mission

Define and test the provider-neutral domain model, status reducer, and observer
contract before any provider or daemon integration proceeds. This phase is the
shared seam all later work consumes, so its merge is a hard freeze gate.

## Ownership and dependencies

**Workstream:** C2

**Exclusive write ownership:**

- `internal/agentgraph/**`
- `internal/provider/provider.go`

**Dependencies:** the accepted decisions in this planning packet and the
generated Codex 0.149 schema. The live E0 capture may validate the design but is
not allowed to mutate it behind other agents' backs.

Do not edit state, history, daemon, RPC, discovery, provider implementations, or
renderers.

## Package boundary

`internal/agentgraph` is pure domain logic:

- no filesystem, process, network, app-server, transcript, state-store, RPC, or
  renderer imports;
- deterministic functions over values;
- safe immutable snapshots at package boundaries;
- injectable clock/freshness values rather than calls to `time.Now()` inside the
  reducer.

`internal/provider/provider.go` is the narrow orchestration seam. It imports the
graph package, but neither package imports `internal/state`.

## Proposed model

Names may be adjusted for Go clarity, but the represented information and axes
are mandatory:

```go
type ProviderKind string // claude | codex

type RuntimeState string
const (
    RuntimeUnknown     RuntimeState = "unknown"
    RuntimeNotLoaded   RuntimeState = "not_loaded"
    RuntimeIdle        RuntimeState = "idle"
    RuntimeActive      RuntimeState = "active"
    RuntimeSystemError RuntimeState = "system_error"
)

type AttentionState string
const (
    AttentionNone      AttentionState = "none"
    AttentionApproval  AttentionState = "approval"
    AttentionUserInput AttentionState = "user_input"
)

type LifecycleState string
// unknown, pending, running, completed, interrupted, errored, shutdown,
// not_found

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
    Usage       Usage // optional/zero when unavailable
}

type Observation struct {
    Provider    ProviderKind
    RootID      string
    Nodes       []Node
    Source      SourceKind
    ObservedAt  time.Time
    FreshUntil  time.Time
    Complete    bool
    Diagnostic  string // in-memory/logging only; never user content
}
```

`SourceKind` distinguishes at least app-server, hook, Claude transcript, Codex
rollout, and restored-last-known state. It is not a confidence number. Source
precedence is explicit in provider adapters and tested there.

Usage fields are optional and additive. Absence must not affect status.

## Graph invariants

- Node IDs are unique within one root observation.
- Exactly one node may have `ID == RootID`; its `ParentID` is empty.
- A child is attached only when its explicit parent chain reaches `RootID`.
- No child is promoted to root because its parent is missing.
- Cycles and duplicate IDs are invalid observations and produce a diagnostic.
- A partial observation may omit previously known nodes, but `Complete` says
  whether omission means deletion/drain or merely unavailable data.
- Completed/interrupted/errored/shutdown nodes may remain for display/history,
  but they are not considered live work by the reducer.
- An observation is a bounded current-session view, not an archive. It contains
  every live descendant and may retain terminal descendants from the current or
  most recently completed root turn. Providers prune older terminal nodes;
  durable history belongs in `internal/history`.
- Freshness is evaluated from the observation timestamp/deadline supplied by the
  caller. An expired observation cannot keep a root green or red.

Provide a normalization/validation function that returns a deterministic node
order: root first, then stable depth-first or parent/name/ID ordering. The choice
must be documented and pinned so state snapshots do not republish due to map
iteration order.

## Reducer contract

The reducer returns a structured summary, not only the legacy string:

```go
type Summary struct {
    Runtime        RuntimeState
    Attention      AttentionState
    LegacyStatus   string // working|idle|permission|delegating|""
    LiveChildren   int
    WaitingNodes   int
    ErrorNodes     int
    Since          time.Time
}
```

Required rules, in priority order:

1. Ignore expired observations for authoritative status.
2. Any live node waiting for approval or user input makes the aggregate
   attention-bearing and legacy `permission`.
3. If both attention kinds exist, preserve both in counts/details; choose a
   deterministic summary priority (`approval` first is recommended) solely for
   compact renderers.
4. Root active with no wait -> `working`.
5. Root idle/not-loaded/unknown with a live active/pending/running descendant ->
   `delegating`.
6. Root idle and no live working/waiting descendant -> `idle`.
7. A completed child does not make a root delegating.
8. A child system error increments error count but does not masquerade as
   permission. A root system error yields an explicit runtime error and empty
   legacy status in v1.
9. `Since` changes only when the derived state changes; repeated identical
   observations preserve it. Implement this as a pure function accepting the
   prior summary and `now`.

## Provider observer seam

The initial interface should support both a polling/file observer and a
long-running event-stream cache:

```go
type RootRef struct {
    PID               int
    StartedAt         time.Time
    Provider          agentgraph.ProviderKind
    ProviderSessionID string
    Transcript        string
    CWD               string
}

type RootKey struct {
    PID       int
    StartedAt time.Time
}

type Observer interface {
    Observe(context.Context, RootRef, time.Time) (agentgraph.Observation, error)
    Updates() <-chan RootKey
    Forget(RootKey)
    Close() error
}
```

Contract details:

- `Observe` may perform provider I/O, so callers must invoke it outside the
  state-store lock.
- `Updates` is a non-blocking, coalesced invalidation signal. It says "observe
  this root again," not "this is the complete state."
- Losing an invalidation is safe because periodic reconciliation remains the
  backstop.
- `Forget` is idempotent and keyed by PID plus start identity to survive PID
  reuse.
- `Close` stops internal goroutines and closes resources exactly once.
- Implementations return observations by value/deep copy; callers cannot race
  an adapter's mutable cache.

If implementation experience proves a method cannot serve both providers, the
coordinator reviews one contract amendment at W0. Provider agents must not fork
the interface independently.

## Tests

Add table-driven tests for:

- every reducer rule and priority combination;
- approval versus user-input preservation;
- two simultaneous waiting children;
- active root plus waiting child;
- idle root plus running child;
- completed, errored, interrupted, and shutdown children;
- pruning terminal children when a new root turn establishes a new cohort;
- expired observation and restored-last-known source;
- duplicate IDs, cycle, orphan, missing root, and deterministic ordering;
- repeated summary preserving `Since` and a real transition resetting it;
- mutation of returned slices/maps not affecting the source snapshot;
- non-blocking update coalescing contract through a small provider test helper.

Prefer a truth-table fixture with human-readable case names over many ad hoc
tests. Every row states root state, child states, expected attention, expected
legacy status, and counts.

## Acceptance and freeze gate

- `go test ./internal/agentgraph ./internal/provider` passes under `-race`.
- Package documentation states all invariants and source/freshness semantics.
- There are no imports from state, history, RPC, daemon, or provider-specific
  packages.
- The API represents both waiting flags and every installed Codex lifecycle
  enum without provider-specific names.
- The coordinator reviews and records the merge commit as the W0 contract-freeze
  baseline.

After freeze, only the coordinator may amend these paths until Wave 1 completes.
