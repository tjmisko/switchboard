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

// PendingPrompt is one writer-owned attention wait. Correlators remain
// in-memory compatibility state and never enter the neutral graph.
type PendingPrompt struct {
	Tool      string
	InputHash string
	Attention agentgraph.AttentionState
	Since     time.Time
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

// Compatibility contains the legacy fields C5/C6 must continue projecting
// while the graph runs in shadow. Pending is keyed by normalized bare writer ID;
// the empty key is the main thread.
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
		Pending:           clonePending(rs.pending),
	}
	projection.PendingWriters = pendingWritersForProjection(rs.pending)
	projection.PendingTool = derivedPendingTool(rs.pending)
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

func restoredPending(restored Compatibility, at time.Time) map[string]PendingPrompt {
	pending := clonePending(restored.Pending)
	if len(pending) == 0 {
		pending = make(map[string]PendingPrompt, len(restored.PendingWriters))
		for _, writer := range restored.PendingWriters {
			if writer == PendingWriterMain {
				writer = ""
			}
			pending[writer] = PendingPrompt{Attention: agentgraph.AttentionApproval, Since: at}
		}
	}
	for writer, prompt := range pending {
		if prompt.Attention == "" || !prompt.Attention.Valid() {
			prompt.Attention = attentionForTool(prompt.Tool)
		}
		if prompt.Since.IsZero() {
			prompt.Since = at
		}
		pending[writer] = prompt
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
