package agentgraph

import (
	"errors"
	"fmt"
	"sort"
)

var (
	// ErrMissingRoot means RootID is empty or has no corresponding node.
	ErrMissingRoot = errors.New("agent graph root is missing")
	// ErrEmptyNodeID means a node has no stable identity.
	ErrEmptyNodeID = errors.New("agent graph node ID is empty")
	// ErrDuplicateNode means two nodes share an ID within one observation.
	ErrDuplicateNode = errors.New("agent graph node ID is duplicated")
	// ErrRootHasParent means the root node has a non-empty ParentID.
	ErrRootHasParent = errors.New("agent graph root has a parent")
	// ErrOrphanNode means a child has no explicit parent chain to RootID.
	ErrOrphanNode = errors.New("agent graph node is orphaned")
	// ErrCycle means a parent chain repeats a node.
	ErrCycle = errors.New("agent graph contains a cycle")
)

// Normalize validates graph structure and returns an immutable-boundary copy
// in deterministic depth-first pre-order. Siblings sort lexicographically by
// nickname, role, and ID. Empty axis values are made explicit as unknown/none.
//
// On failure, the returned observation has no usable nodes and carries a
// content-free structural diagnostic. The error wraps one of the Err* values.
func Normalize(observation Observation) (Observation, error) {
	out := observation.Clone()
	if out.RootID == "" {
		return invalid(out, ErrMissingRoot)
	}

	byID := make(map[string]Node, len(out.Nodes))
	for _, node := range out.Nodes {
		if node.ID == "" {
			return invalid(out, ErrEmptyNodeID)
		}
		if _, exists := byID[node.ID]; exists {
			return invalid(out, fmt.Errorf("%w: %q", ErrDuplicateNode, node.ID))
		}
		node.Runtime = canonicalRuntime(node.Runtime)
		node.Attention = canonicalAttention(node.Attention)
		node.Lifecycle = canonicalLifecycle(node.Lifecycle)
		byID[node.ID] = node
	}

	root, exists := byID[out.RootID]
	if !exists {
		return invalid(out, fmt.Errorf("%w: %q", ErrMissingRoot, out.RootID))
	}
	if root.ParentID != "" {
		return invalid(out, fmt.Errorf("%w: %q", ErrRootHasParent, root.ID))
	}

	for id, node := range byID {
		if id == out.RootID {
			continue
		}
		if node.ParentID == "" {
			return invalid(out, fmt.Errorf("%w: %q", ErrOrphanNode, node.ID))
		}
		seen := map[string]struct{}{id: {}}
		parentID := node.ParentID
		for parentID != out.RootID {
			if _, repeated := seen[parentID]; repeated {
				return invalid(out, fmt.Errorf("%w at node %q", ErrCycle, node.ID))
			}
			seen[parentID] = struct{}{}
			parent, ok := byID[parentID]
			if !ok || parentID == "" {
				return invalid(out, fmt.Errorf("%w: %q", ErrOrphanNode, node.ID))
			}
			parentID = parent.ParentID
		}
	}

	children := make(map[string][]Node, len(byID))
	for id, node := range byID {
		if id != out.RootID {
			children[node.ParentID] = append(children[node.ParentID], node)
		}
	}
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			left, right := children[parentID][i], children[parentID][j]
			if left.Nickname != right.Nickname {
				return left.Nickname < right.Nickname
			}
			if left.Role != right.Role {
				return left.Role < right.Role
			}
			return left.ID < right.ID
		})
	}

	ordered := make([]Node, 0, len(byID))
	ordered = append(ordered, root)
	var appendChildren func(string)
	appendChildren = func(parentID string) {
		for _, child := range children[parentID] {
			ordered = append(ordered, child)
			appendChildren(child.ID)
		}
	}
	appendChildren(out.RootID)
	out.Nodes = ordered
	return out, nil
}

func invalid(out Observation, err error) (Observation, error) {
	out.Nodes = nil
	if out.Diagnostic == "" {
		out.Diagnostic = err.Error()
	} else {
		out.Diagnostic += "; " + err.Error()
	}
	return out, err
}

func canonicalRuntime(runtime RuntimeState) RuntimeState {
	if runtime == "" || !runtime.Valid() {
		return RuntimeUnknown
	}
	return runtime
}

func canonicalAttention(attention AttentionState) AttentionState {
	if attention == "" || !attention.Valid() {
		return AttentionNone
	}
	return attention
}

func canonicalLifecycle(lifecycle LifecycleState) LifecycleState {
	if lifecycle == "" || !lifecycle.Valid() {
		return LifecycleUnknown
	}
	return lifecycle
}
