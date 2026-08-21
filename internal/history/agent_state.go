package history

import (
	"sort"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// AgentStateContext supplies root-session metadata that is not part of the
// provider graph. SessionID and Agent on emitted events come from the normalized
// observation, keeping one canonical provider/root identity.
type AgentStateContext struct {
	PID     int
	CWD     string
	Project string
}

// AgentStateProjector turns successive immutable observations into canonical
// agent_state history edges. It owns a root-lifetime current-state/dedupe lane
// per provider root and is safe for concurrent reconcile triggers. Call Forget
// when the root process lifetime ends.
type AgentStateProjector struct {
	mu    sync.Mutex
	roots map[agentStateRootKey]*agentStateRoot
}

type agentStateRootKey struct {
	provider agentgraph.ProviderKind
	rootID   string
}

type agentStateRoot struct {
	nodes map[string]trackedAgentNode
	seen  map[agentStateDedupeKey]struct{}
}

type trackedAgentNode struct {
	node  agentgraph.Node
	since time.Time
}

// agentStateDedupeKey is deliberately axis-specific. A reconnect can replay an
// already-recorded tuple as an apparent unknown->current edge; including root,
// node, axis, target, and the provider transition timestamp recognizes that
// replay without suppressing a later return to the same value.
type agentStateDedupeKey struct {
	root        agentStateRootKey
	nodeID      string
	axis        string
	target      string
	transitionN int64
}

// NewAgentStateProjector constructs an empty canonical transition projector.
func NewAgentStateProjector() *AgentStateProjector {
	return &AgentStateProjector{roots: make(map[agentStateRootKey]*agentStateRoot)}
}

// Project diffs one fresh observation against its root lane. Invalid graphs
// return an error and expired graphs return no events. Partial omissions retain
// prior nodes; only a complete observation can transition an omitted node to
// not_found. Events are deterministic: normalized graph preorder for present
// nodes, then stable ID order for authoritative removals.
func (p *AgentStateProjector) Project(ctx AgentStateContext, observation agentgraph.Observation, now time.Time) ([]Event, error) {
	normalized, err := agentgraph.Normalize(observation)
	if err != nil {
		return nil, err
	}
	if !normalized.Fresh(now) {
		return nil, nil
	}
	if p == nil {
		return nil, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.roots == nil {
		p.roots = make(map[agentStateRootKey]*agentStateRoot)
	}
	rootKey := agentStateRootKey{provider: normalized.Provider, rootID: normalized.RootID}
	root := p.roots[rootKey]
	if root == nil {
		root = &agentStateRoot{
			nodes: make(map[string]trackedAgentNode),
			seen:  make(map[agentStateDedupeKey]struct{}),
		}
		p.roots[rootKey] = root
	}

	present := make(map[string]struct{}, len(normalized.Nodes))
	events := make([]Event, 0, len(normalized.Nodes))
	for _, node := range normalized.Nodes {
		present[node.ID] = struct{}{}
		prior, exists := root.nodes[node.ID]
		from := unknownAgentNode(node.ID, node.ParentID)
		if exists {
			from = prior.node
		}
		transitionAt := agentNodeTransitionTime(node, normalized.ObservedAt, !exists)
		if event, emit := p.transition(rootKey, root, ctx, normalized.Source, from, node, prior.since, transitionAt); emit {
			events = append(events, event)
		}
		root.nodes[node.ID] = trackedAgentNode{node: node, since: nextAgentStateSince(from, node, prior.since, transitionAt)}
	}

	if normalized.Complete {
		var missing []string
		for id, prior := range root.nodes {
			if _, exists := present[id]; exists || prior.node.Lifecycle == agentgraph.LifecycleNotFound {
				continue
			}
			missing = append(missing, id)
		}
		sort.Strings(missing)
		for _, id := range missing {
			prior := root.nodes[id]
			next := prior.node
			next.Runtime = agentgraph.RuntimeUnknown
			next.Attention = agentgraph.AttentionNone
			next.Lifecycle = agentgraph.LifecycleNotFound
			next.UpdatedAt = normalized.ObservedAt
			transitionAt := normalized.ObservedAt
			if event, emit := p.transition(rootKey, root, ctx, normalized.Source, prior.node, next, prior.since, transitionAt); emit {
				events = append(events, event)
			}
			root.nodes[id] = trackedAgentNode{node: next, since: nextAgentStateSince(prior.node, next, prior.since, transitionAt)}
		}
	}
	// A snapshot can discover an older child transition beside a newer root
	// transition. Preserve event-time order for append-only history readers while
	// retaining normalized graph/removal order for equal timestamps.
	sort.SliceStable(events, func(i, j int) bool { return events[i].Ts.Before(events[j].Ts) })
	return events, nil
}

// Forget closes the in-memory state and dedupe lane for one root lifetime. It
// is idempotent; a later observation for the same provider/root begins a new
// lifetime and therefore emits initial canonical state again.
func (p *AgentStateProjector) Forget(provider agentgraph.ProviderKind, rootID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.roots, agentStateRootKey{provider: provider, rootID: rootID})
	p.mu.Unlock()
}

func (p *AgentStateProjector) transition(rootKey agentStateRootKey, root *agentStateRoot, ctx AgentStateContext,
	source agentgraph.SourceKind, from, to agentgraph.Node, since, at time.Time) (Event, bool) {
	changed := changedAgentAxes(from, to)
	if len(changed) == 0 {
		return Event{}, false
	}
	at = at.UTC()
	emit := false
	for _, axis := range changed {
		key := agentStateDedupeKey{
			root:        rootKey,
			nodeID:      to.ID,
			axis:        axis.name,
			target:      axis.target,
			transitionN: at.UnixNano(),
		}
		if _, duplicate := root.seen[key]; !duplicate {
			emit = true
		}
		root.seen[key] = struct{}{}
	}
	if !emit {
		return Event{}, false
	}
	return Event{
		Ts:        at,
		Type:      EventAgentState,
		SessionID: rootKey.rootID,
		PID:       ctx.PID,
		Agent:     string(rootKey.provider),
		Project:   ctx.Project,
		CWD:       ctx.CWD,

		ThreadID:       to.ID,
		ParentThreadID: to.ParentID,
		Nickname:       to.Nickname,
		Role:           to.Role,
		FromRuntime:    from.Runtime,
		ToRuntime:      to.Runtime,
		FromAttention:  from.Attention,
		ToAttention:    to.Attention,
		FromLifecycle:  from.Lifecycle,
		ToLifecycle:    to.Lifecycle,
		Source:         source,
		DurPrevMs:      agentStateHeldMs(since, at),
	}, true
}

type changedAgentAxis struct {
	name   string
	target string
}

func changedAgentAxes(from, to agentgraph.Node) []changedAgentAxis {
	var changed []changedAgentAxis
	if from.Runtime != to.Runtime {
		changed = append(changed, changedAgentAxis{name: "runtime", target: string(to.Runtime)})
	}
	if from.Attention != to.Attention {
		changed = append(changed, changedAgentAxis{name: "attention", target: string(to.Attention)})
	}
	if from.Lifecycle != to.Lifecycle {
		changed = append(changed, changedAgentAxis{name: "lifecycle", target: string(to.Lifecycle)})
	}
	return changed
}

func unknownAgentNode(id, parentID string) agentgraph.Node {
	return agentgraph.Node{
		ID:        id,
		ParentID:  parentID,
		Runtime:   agentgraph.RuntimeUnknown,
		Attention: agentgraph.AttentionNone,
		Lifecycle: agentgraph.LifecycleUnknown,
	}
}

func agentNodeTransitionTime(node agentgraph.Node, observedAt time.Time, initial bool) time.Time {
	if node.Lifecycle.Terminal() && !node.CompletedAt.IsZero() {
		return node.CompletedAt
	}
	if !node.UpdatedAt.IsZero() {
		return node.UpdatedAt
	}
	if initial && !node.StartedAt.IsZero() {
		return node.StartedAt
	}
	return observedAt
}

func sameAgentAxes(left, right agentgraph.Node) bool {
	return left.Runtime == right.Runtime && left.Attention == right.Attention && left.Lifecycle == right.Lifecycle
}

func nextAgentStateSince(from, to agentgraph.Node, prior, transitionAt time.Time) time.Time {
	if sameAgentAxes(from, to) {
		return prior
	}
	return transitionAt
}

func agentStateHeldMs(since, now time.Time) int64 {
	if since.IsZero() || now.Before(since) {
		return 0
	}
	return HeldMs(since, now)
}
