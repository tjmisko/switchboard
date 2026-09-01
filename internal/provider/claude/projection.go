package claude

import (
	"sort"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
)

// PendingWriterMain is the compatibility wire spelling for the empty main-
// thread writer key.
const PendingWriterMain = "main"

// PendingPrompt is one call's attention wait, owned by the writer that raised
// it. A writer may hold several at once — one assistant turn can dispatch
// parallel gated calls — so the writer routes evidence while the individual
// call closes the prompt. Correlators remain in-memory compatibility state and
// never enter the neutral graph.
//
// The first four fields are the prompt's IDENTITY: they are exactly what the
// PermissionRequest edge carried, and promptIdentity compares them. The latch
// fields below are learned later, from the writer's own transcript, and are
// deliberately NOT part of that identity — a prompt that has since bound its
// call id is still the same prompt, so a verbatim hook redelivery must dedupe
// against it and the Observe merge must still recognize it.
type PendingPrompt struct {
	Tool      string
	InputHash string
	Attention agentgraph.AttentionState
	Since     time.Time

	// CallID is Claude Code's exact identity for the gated call, latched lazily
	// on an Observe tick (PendingCall) because PermissionRequest carries none. It
	// is meaningful only while Latch is CallLatchBound.
	CallID string
	Latch  CallLatchState
	// Restored marks a prompt rebuilt from the persisted legacy block rather than
	// opened by a hook. Such a record stands for a writer's residual RED, not for
	// one call — the block carries one prompt per writer, so a writer that went
	// down blocked on three calls comes back holding one (see restoredPending).
	// Binding a call id to it would let that one id clear a red three real calls
	// are still holding, so the latch refuses. It clears when the schema persists
	// the set instead of the scalar (plan Phase 4 step 11).
	Restored bool
}

// CallLatchState is how far the lazy call-identity latch has got with one
// prompt. It is tracked explicitly rather than inferred from an empty CallID
// because "no id yet, look again next tick" and "this prompt can never be
// identified" are different states with opposite retry behaviour, and folding
// both onto "" would either re-read an ambiguous tail forever or stop retrying a
// prompt whose tool_use simply had not flushed yet.
type CallLatchState uint8

const (
	// CallLatchUnbound — no id yet. The writer's tail has not shown exactly one
	// unmatched tool_use of this prompt's tool, usually because the ~5 s flush has
	// not landed. Retried on every Observe tick.
	CallLatchUnbound CallLatchState = iota
	// CallLatchBound — CallID names the exact call this prompt gates.
	CallLatchBound
	// CallLatchAmbiguous — the writer's tail held two or more unmatched calls of
	// this tool, so nothing in the file distinguishes this prompt's from a
	// sibling's. Terminal: the candidate set shrinks as siblings complete, but
	// nothing records WHICH one shrank, so a later unique read is exactly as
	// likely to name the sibling. The prompt falls back to the (tool, hash) rule
	// forever, which is a stale red rather than the missed red a wrong bind buys.
	CallLatchAmbiguous
)

// promptIdentity is the hook-derived identity of a prompt — what the
// PermissionRequest edge itself carried, and nothing learned since. Two prompts
// of one writer can never share it: openPendingPrompt dedupes on exactly these
// fields, so it addresses a single record within a writer's open set.
type promptIdentity struct {
	Tool      string
	InputHash string
	Attention agentgraph.AttentionState
	Since     time.Time
}

func (p PendingPrompt) identity() promptIdentity {
	return promptIdentity{Tool: p.Tool, InputHash: p.InputHash, Attention: p.Attention, Since: p.Since}
}

// DrainLegacyEvents returns and forgets the exact-once Claude fanout/workflow
// history events produced by successful Observe calls for key. It preserves the
// existing event stream during shadow migration without putting history details
// into the neutral graph.
func (o *Observer) DrainLegacyEvents(key provider.RootKey) []history.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	rs := o.roots[key]
	if o.closed || rs == nil || len(rs.legacyEvents) == 0 {
		return nil
	}
	events := append([]history.Event(nil), rs.legacyEvents...)
	rs.legacyEvents = nil
	return events
}

// DrainResolutionRule returns and forgets the rule id of the transcript
// resolution the most recent Observe applied for key, or "" when that Observe
// resolved nothing. It is the Observe-tick counterpart of HookResult.Rule, and
// it is drained rather than read so one resolution can explain at most one
// recorded transition: a rule that outlived its tick would eventually be
// stamped on an edge it had nothing to do with, which is worse than the blanket
// id it replaces — a wrong explanation costs more than an absent one.
func (o *Observer) DrainResolutionRule(key provider.RootKey) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	rs := o.roots[key]
	if o.closed || rs == nil {
		return ""
	}
	rule := rs.resolvedRule
	rs.resolvedRule = ""
	return rule
}

// DrainPromptDiagnostics returns and forgets the bounded, content-free
// diagnostic categories the most recent Observe calls for key produced while
// latching call identity. Drained rather than read for DrainResolutionRule's
// reason: a counter that is read twice reports an event that happened once, and
// these exist to be counted honestly — they are how anyone can tell whether the
// id fast path is reachable in practice, and whether the uniqueness the lifted
// fanout floor rests on ever fails (P3, askuserquestion-model-plan.md §5).
func (o *Observer) DrainPromptDiagnostics(key provider.RootKey) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	rs := o.roots[key]
	if o.closed || rs == nil || len(rs.promptDiagnostics) == 0 {
		return nil
	}
	categories := rs.promptDiagnostics
	rs.promptDiagnostics = nil
	return categories
}

// Compatibility contains the legacy fields C5/C6 must continue projecting
// while the graph runs in shadow. Pending is keyed by normalized bare writer ID;
// the empty key is the main thread. It carries at most one prompt per writer —
// the legacy block's shape — while the adapter tracks the writer's whole open
// set; the KEY SET, which is what the wire and the chip consume, is the same
// either way (see projectedPending).
type Compatibility struct {
	SessionID         string
	Transcript        string
	Status            string
	StatusSince       time.Time
	InFlightSubagents int
	Workflows         []fanout.Workflow
	Pending           map[string]PendingPrompt
	PendingWriters    []string
	PendingTool       string
}

// Clone returns a fully detached compatibility view.
func (c Compatibility) Clone() Compatibility {
	clone := c
	clone.Workflows = append([]fanout.Workflow(nil), c.Workflows...)
	clone.PendingWriters = append([]string(nil), c.PendingWriters...)
	clone.Pending = clonePending(c.Pending)
	return clone
}

// Projection returns the latest detached compatibility view for key.
func (o *Observer) Projection(key provider.RootKey) Compatibility {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.roots[key] == nil {
		return Compatibility{}
	}
	return o.roots[key].projection.Clone()
}

// Restore hydrates the adapter from a persisted legacy compatibility block
// before the first authoritative transcript observation. Persisted writer keys
// retain attention ownership; missing correlators are intentionally not
// invented, so those prompts resolve through their own transcripts rather than
// the hook-speed match path.
func (o *Observer) Restore(root provider.RootRef, restored Compatibility, at time.Time) (agentgraph.Observation, error) {
	if err := validateRoot(root); err != nil {
		return agentgraph.Observation{}, err
	}
	if at.IsZero() {
		at = time.Now()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return agentgraph.Observation{}, ErrClosed
	}
	rs := o.ensureRootLocked(root)
	rs.runtime = runtimeFromLegacy(restored.Status)
	rs.runtimeAt = restored.StatusSince
	if rs.runtimeAt.IsZero() {
		rs.runtimeAt = at
	}
	rs.pending = restoredPending(restored, at)
	rs.fanout.InFlight = restored.InFlightSubagents
	rs.fanout.Workflows = append([]fanout.Workflow(nil), restored.Workflows...)
	for writer := range rs.pending {
		if writer != "" {
			rs.overlays[writer] = childOverlay{Runtime: agentgraph.RuntimeIdle, UpdatedAt: at}
		}
	}
	observation, err := o.rebuildLocked(rs, at, agentgraph.SourceRestoredLastKnown, false)
	if err != nil {
		return observation, err
	}
	// A pre-graph mirror can carry a delegating summary/count without stable child
	// IDs. Preserve that legacy projection until Observe discovers the exact nodes;
	// never manufacture anonymous graph children to force reducer parity.
	if restored.Status != "" {
		rs.projection.Status = restored.Status
	}
	if !restored.StatusSince.IsZero() {
		rs.projection.StatusSince = restored.StatusSince
		if restored.Status == rs.priorSummary.LegacyStatus {
			rs.priorSummary.Since = restored.StatusSince
		}
	}
	return observation.Clone(), nil
}

func projectCompatibility(rs *rootState, summary agentgraph.Summary) Compatibility {
	statusSince := summary.Since
	if rs.projection.Status == summary.LegacyStatus && !rs.projection.StatusSince.IsZero() {
		// Legacy status_since dates chip-color transitions, not graph-detail
		// changes such as a second blocked writer or another live child.
		statusSince = rs.projection.StatusSince
	}
	projection := Compatibility{
		SessionID: rs.ref.ProviderSessionID, Transcript: rs.ref.Transcript,
		Status: summary.LegacyStatus, StatusSince: statusSince,
		InFlightSubagents: rs.fanout.InFlight,
		Workflows:         append([]fanout.Workflow(nil), rs.fanout.Workflows...),
		Pending:           projectedPending(rs.pending),
	}
	projection.PendingWriters = pendingWritersForProjection(projection.Pending)
	projection.PendingTool = derivedPendingTool(projection.Pending)
	return projection
}

// projectedPending collapses each writer's open set to the one prompt the legacy
// compatibility block can carry. The key set — which is what the wire, the chip
// and the restore path actually consume — is unchanged, and for the single-prompt
// case the value is byte-identical to the pre-container projection.
//
// The newest prompt is the one kept. Only one survives a daemon restart, and the
// newest Since is the one that keeps the restored red alive longest under the
// stale backstop; keeping the oldest would let a restart shorten a live red.
func projectedPending(pending map[string][]PendingPrompt) map[string]PendingPrompt {
	projection := make(map[string]PendingPrompt, len(pending))
	for writer, prompts := range pending {
		if len(prompts) == 0 {
			continue
		}
		projection[writer] = prompts[len(prompts)-1]
	}
	return projection
}

func pendingWritersForProjection(pending map[string]PendingPrompt) []string {
	if len(pending) == 0 {
		return nil
	}
	writers := make([]string, 0, len(pending))
	for writer := range pending {
		if writer == "" {
			writer = PendingWriterMain
		}
		writers = append(writers, writer)
	}
	sort.Strings(writers)
	return writers
}

func sortedPendingWriters(pending map[string]PendingPrompt) []string {
	writers := make([]string, 0, len(pending))
	for writer := range pending {
		writers = append(writers, writer)
	}
	sort.Strings(writers)
	return writers
}

func derivedPendingTool(pending map[string]PendingPrompt) string {
	if prompt, ok := pending[""]; ok {
		return prompt.Tool
	}
	writers := sortedPendingWriters(pending)
	if len(writers) == 0 {
		return ""
	}
	return pending[writers[0]].Tool
}

// restoredPending rebuilds the in-memory open sets from a persisted legacy
// block. That block carries one prompt per writer, so a writer that was blocked
// on three parallel calls comes back holding ONE, and the other two are gone —
// M2's missed RED, reinstated for the window between the restart and whatever
// transcript evidence resolves the survivor.
//
// Nothing re-learns them from hooks. PermissionRequest fires exactly once per
// call (docs/claude-code-hook-schema.md §2), so no later edge re-opens a prompt
// that was already open when the daemon went down; the writer's transcript is
// the only recovery path, and its rules resolve the whole set at once rather
// than naming the forgotten calls. Persisting the SET instead of the scalar is
// the fix, and it belongs with the state-schema change already scheduled for
// Phase 4 step 11 (askuserquestion-model-plan.md §4).
//
// Until then a restored record is one writer's residual red, NOT a stand-in for
// its open set. Anything that later binds a call identity to it — the lazy latch
// in Phase 4 step 7 — must not then let that one id clear the writer's red, or
// the restore path converts today's honest single red into a green with real
// calls still blocking.
func restoredPending(restored Compatibility, at time.Time) map[string][]PendingPrompt {
	prompts := clonePending(restored.Pending)
	if len(prompts) == 0 {
		prompts = make(map[string]PendingPrompt, len(restored.PendingWriters))
		for _, writer := range restored.PendingWriters {
			if writer == PendingWriterMain {
				writer = ""
			}
			prompts[writer] = PendingPrompt{Attention: agentgraph.AttentionApproval, Since: at}
		}
	}
	pending := make(map[string][]PendingPrompt, len(prompts))
	for writer, prompt := range prompts {
		if prompt.Attention == "" || !prompt.Attention.Valid() {
			prompt.Attention = attentionForTool(prompt.Tool)
		}
		if prompt.Since.IsZero() {
			prompt.Since = at
		}
		prompt.CallID, prompt.Latch, prompt.Restored = "", CallLatchUnbound, true
		pending[writer] = []PendingPrompt{prompt}
	}
	return pending
}

func runtimeFromLegacy(status string) agentgraph.RuntimeState {
	switch status {
	case agentgraph.LegacyWorking:
		return agentgraph.RuntimeActive
	case agentgraph.LegacyIdle, agentgraph.LegacyDelegating, agentgraph.LegacyPermission:
		return agentgraph.RuntimeIdle
	default:
		return agentgraph.RuntimeUnknown
	}
}
