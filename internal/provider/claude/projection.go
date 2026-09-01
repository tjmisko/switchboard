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
// fields below are learned either from a unique PreToolUse→PermissionRequest
// join at open or later from the writer's own transcript. They are deliberately
// NOT part of prompt identity — a prompt that has bound its call id is still the
// same prompt, so a verbatim hook redelivery must dedupe against it and the
// Observe merge must still recognize it.
type PendingPrompt struct {
	Tool      string
	InputHash string
	Attention agentgraph.AttentionState
	Since     time.Time

	// CallID is Claude Code's exact identity for the gated call, joined from a
	// matcher-limited PreToolUse at red onset when unique or latched lazily on an
	// Observe tick otherwise. It is meaningful only while Latch is CallLatchBound.
	CallID string
	Latch  CallLatchState
	// LatchAt is the instant the current latch state was reached. Only the
	// CallLatchProposed value is read: a proposal may not be confirmed by a read
	// that lands within callLatchConfirmGrace of it, because two reads inside one
	// message's flush gap see the same partial file and agreeing about it proves
	// nothing (see CallLatchProposed).
	LatchAt time.Time
	// Residual marks a prompt rebuilt from a persisted block that could carry only
	// ONE prompt per writer. Such a record stands for a writer's leftover RED rather
	// than for one call — a writer that went down blocked on three calls comes back
	// holding a single record — so binding a call id to it would let that one id
	// clear a red three real calls are still holding, and the latch refuses
	// (latchablePrompt).
	//
	// It is set for a prompt rebuilt from the legacy scalar and NOT for one rebuilt
	// from a per-call record, which names exactly one call and may latch like any
	// other. That distinction is what keeps an old state.json restoring as
	// conservatively as it always did while a new one restores the real set.
	Residual bool
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
	// CallLatchProposed — one candidate survived this tick, and CallID holds it
	// pending a LATER read that names the same one. It is not an identity yet and
	// nothing may match on it.
	//
	// The confirmation exists because a single read's uniqueness is not evidence
	// of uniqueness. One assistant message's parallel tool_use blocks are separate
	// JSONL entries written 0.5–1.5 s apart, so a read landing inside that gap
	// sees a gated call's auto-approved sibling ALONE and reads it as unique. A
	// read taken after the gap sees both and refuses.
	//
	// "Later" is wall-clock and not merely "the next call", which is the part that
	// is easy to get wrong: ApplyHook signals the coordinator, so every hook from
	// ANY writer schedules an Observe for this root, and a fanned-out session can
	// deliver two reads milliseconds apart. Two such reads see the same partial
	// file, so agreement between them is not evidence of anything —
	// callLatchConfirmGrace is what makes the second read a genuinely later view.
	// The cost is one extra tick against 45–300 s of measured think-time; the
	// alternative is a wrong bind, which is a missed RED.
	CallLatchProposed
	// CallLatchContested — two writers of one session offered the same call id, so
	// the id cannot name a writer and no tiebreak has evidence behind it. Terminal
	// for the same reason as CallLatchAmbiguous, and counted separately: it is the
	// runtime half of the P3 uniqueness assertion the lifted fanout floor rests on
	// (askuserquestion-model-plan.md §5), and a terminal state is what keeps that
	// counter one-per-prompt instead of one-per-tick.
	CallLatchContested
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
// diagnostic categories the most recent hooks/Observe calls for key produced
// while staging, joining, or latching call identity. Drained rather than read
// for DrainResolutionRule's reason: a counter that is read twice reports an
// event that happened once. These exist to be counted honestly — they are how
// anyone can tell whether the id fast path is reachable in practice, and
// whether the uniqueness the lifted fanout floor rests on ever fails (P3,
// askuserquestion-model-plan.md §5).
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
//
// PendingSets carries that whole open set beside it, unabridged. It is additive:
// every existing consumer of Pending is unchanged, and a caller that supplies only
// Pending (a pre-record state.json, or a test written against the legacy shape)
// restores exactly as it did before.
type Compatibility struct {
	SessionID         string
	Transcript        string
	Status            string
	StatusSince       time.Time
	InFlightSubagents int
	Workflows         []fanout.Workflow
	Pending           map[string]PendingPrompt
	PendingSets       map[string][]PendingPrompt
	PendingWriters    []string
	PendingTool       string
}

// Clone returns a fully detached compatibility view.
func (c Compatibility) Clone() Compatibility {
	clone := c
	clone.Workflows = append([]fanout.Workflow(nil), c.Workflows...)
	clone.PendingWriters = append([]string(nil), c.PendingWriters...)
	clone.Pending = clonePending(c.Pending)
	clone.PendingSets = clonePendingSets(c.PendingSets)
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
	clear(rs.promptAnchors)
	clear(rs.preToolCandidates)
	clear(rs.preToolContested)
	for writer, prompts := range rs.pending {
		// The anchor is the RESTART instant, never a restored onset. Every whole-file
		// resolution rule dates from it, and a pre-restart onset would make each of
		// that writer's pre-restart entries read as evidence the wait ended — the
		// missed RED the hydrate-time re-stamp has always existed to prevent. A
		// restored prompt keeps its own onset in Since regardless, which is what dates
		// it against its writer's transcript for the call latch.
		rs.promptAnchors[writer] = writerResolutionAnchor(at, prompts)
	}
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
		PendingSets:       clonePendingSets(rs.pending),
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
// The newest prompt is the one kept. The set itself now survives a restart in
// PendingSets, so this scalar is no longer what a restore reads — but it is still
// what a reader that knows only the legacy shape sees, and the newest Since is the
// one that keeps such a red alive longest under the stale backstop, where keeping
// the oldest would shorten a live red.
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

// restoredPending rebuilds the in-memory open sets from a persisted block.
//
// OWNERSHIP is taken from the widest evidence the block carries — the legacy
// per-writer map, its wire key set, and the per-call records — because losing a
// writer is the unrecoverable error: PermissionRequest fires exactly once per call
// (docs/claude-code-hook-schema.md §2), so no later hook re-opens a prompt that was
// already open when the daemon went down, and a writer dropped here is red nothing
// can raise again.
//
// The SET is taken from PendingSets when the block carries records for that
// writer, and that is the whole point of this phase. A block that carries only the
// legacy scalar restores exactly ONE prompt per writer, so a writer that went down
// blocked on three parallel calls came back holding one; answering the survivor
// then took the chip green with two calls still blocking — M2's missed RED,
// manufactured by the restart itself. Records close that.
//
// A prompt rebuilt from the scalar is marked Residual: it stands for a writer's
// leftover red, not for one call, so nothing may bind a call identity to it (see
// PendingPrompt.Residual). A prompt rebuilt from a record is NOT residual — it
// names one call — so it may latch, and its own answer can close it without
// touching its writer's other prompts.
//
// Two clocks, deliberately. Since is the record's own onset, which is what dates a
// prompt against its writer's transcript and lets the latch tell the call it gates
// from an older dispatch (ownableCall). The clock the WHOLE-FILE rules run from is
// the restart instant, and it lives on the writer's resolution anchor, seeded in
// Restore — keeping a pre-restart onset there would make every pre-restart
// assistant entry read as "this writer resumed" and clear a red that was live
// across the restart.
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
	// A writer named only by the records still owns its prompts. The placeholder is
	// never read — the set branch below replaces it — it only puts that writer in
	// the key set the loop walks.
	for writer := range restored.PendingSets {
		if _, owned := prompts[writer]; !owned {
			prompts[writer] = PendingPrompt{Since: at}
		}
	}
	pending := make(map[string][]PendingPrompt, len(prompts))
	for writer, prompt := range prompts {
		if set := restored.PendingSets[writer]; len(set) > 0 {
			pending[writer] = restoredPromptSet(set, at)
			continue
		}
		pending[writer] = []PendingPrompt{restoredPrompt(prompt, at, true)}
	}
	invalidateRestoredCallIDCollisions(pending)
	return pending
}

// A duplicated persisted id means the mirror cannot prove which prompt owns
// the call. Refuse every claim instead of letting one result clear two prompts.
func invalidateRestoredCallIDCollisions(pending map[string][]PendingPrompt) {
	claims := make(map[string]int)
	for _, prompts := range pending {
		for _, prompt := range prompts {
			if prompt.CallID != "" {
				claims[prompt.CallID]++
			}
		}
	}
	for writer, prompts := range pending {
		for i := range prompts {
			if prompts[i].CallID != "" && claims[prompts[i].CallID] > 1 {
				prompts[i].CallID = ""
				prompts[i].Latch = CallLatchContested
			}
		}
		pending[writer] = prompts
	}
}

// restoredPromptSet rebuilds one writer's whole open set from its records,
// oldest-first, and capped exactly as a live set is: a mirror that somehow carries
// more prompts than a writer can hold must not restore past the ceiling the
// append path enforces.
func restoredPromptSet(set []PendingPrompt, at time.Time) []PendingPrompt {
	prompts := make([]PendingPrompt, 0, len(set))
	for _, prompt := range set {
		prompts = append(prompts, restoredPrompt(prompt, at, false))
	}
	sort.SliceStable(prompts, func(i, j int) bool { return prompts[i].Since.Before(prompts[j].Since) })
	if len(prompts) > maxPendingPromptsPerWriter {
		prompts = prompts[len(prompts)-maxPendingPromptsPerWriter:]
	}
	return prompts
}

// restoredPrompt normalizes one rebuilt prompt. A confirmed persisted call id
// stays bound; every other latch state resets. Attention is repaired rather than
// trusted.
//
// The attention repair rejects AttentionNone as well as an unset or unknown value,
// which Valid() alone would accept: "none" is a colourless prompt, and a restored
// prompt that folds to no attention is a red that comes back green. Falling through
// to the tool's own kind can only ever restore a colour.
func restoredPrompt(prompt PendingPrompt, at time.Time, residual bool) PendingPrompt {
	if prompt.Attention != agentgraph.AttentionApproval && prompt.Attention != agentgraph.AttentionUserInput {
		prompt.Attention = attentionForTool(prompt.Tool)
	}
	if prompt.Since.IsZero() {
		prompt.Since = at
	}
	if prompt.CallID != "" && !residual {
		prompt.Latch = CallLatchBound
	} else {
		prompt.CallID, prompt.Latch = "", CallLatchUnbound
	}
	prompt.LatchAt, prompt.Residual = time.Time{}, residual
	return prompt
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
