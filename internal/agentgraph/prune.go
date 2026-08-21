package agentgraph

// PruneTerminal returns a normalized detached observation containing the root,
// every non-terminal descendant, every terminal node named in retain, and all
// ancestors required to keep those nodes explicitly attached to the root.
//
// Provider adapters determine cohort membership from their own authoritative
// turn/artifact evidence and pass the terminal IDs for the current or most
// recently completed root turn. Unknown attribution should be bounded by the
// provider rather than guessed here. The input observation and retain map are
// never mutated or retained.
func PruneTerminal(observation Observation, retain map[string]struct{}) (Observation, error) {
	normalized, err := Normalize(observation)
	if err != nil {
		return normalized, err
	}

	byID := make(map[string]Node, len(normalized.Nodes))
	keep := make(map[string]bool, len(normalized.Nodes))
	for _, node := range normalized.Nodes {
		byID[node.ID] = node
		if node.ID == normalized.RootID || !node.Lifecycle.Terminal() {
			keep[node.ID] = true
		}
		if _, retained := retain[node.ID]; retained {
			keep[node.ID] = true
		}
	}

	for id := range keep {
		for parentID := byID[id].ParentID; parentID != ""; parentID = byID[parentID].ParentID {
			keep[parentID] = true
		}
	}

	pruned := normalized.Clone()
	pruned.Nodes = pruned.Nodes[:0]
	for _, node := range normalized.Nodes {
		if keep[node.ID] {
			pruned.Nodes = append(pruned.Nodes, node)
		}
	}
	return Normalize(pruned)
}
