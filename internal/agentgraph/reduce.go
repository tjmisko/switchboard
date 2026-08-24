package agentgraph

import "time"

// Reduce derives a structured and legacy summary as a pure function of an
// observation, the prior summary, and a caller-supplied clock value. Source is
// deliberately ignored: a fresh restored-last-known observation reduces the
// same way as any other source until its FreshUntil deadline.
func Reduce(observation Observation, prior Summary, now time.Time) Summary {
	next := Summary{Runtime: RuntimeUnknown, Attention: AttentionNone}
	if !observation.Fresh(now) {
		return stampSince(next, prior, now)
	}

	normalized, err := Normalize(observation)
	if err != nil {
		return stampSince(next, prior, now)
	}
	root := normalized.Nodes[0]
	next.Runtime = root.Runtime

	workingDescendant := false
	for _, node := range normalized.Nodes {
		isRoot := node.ID == normalized.RootID
		live := isRoot || PositivelyLive(node)
		if !isRoot && live {
			next.LiveChildren++
			if node.Runtime == RuntimeActive || node.Lifecycle == LifecyclePending || node.Lifecycle == LifecycleRunning {
				workingDescendant = true
			}
		}
		if live {
			switch node.Attention {
			case AttentionApproval:
				next.ApprovalNodes++
			case AttentionUserInput:
				next.UserInputNodes++
			}
		}
		if node.Runtime == RuntimeSystemError || node.Lifecycle == LifecycleErrored {
			next.ErrorNodes++
		}
	}
	next.WaitingNodes = next.ApprovalNodes + next.UserInputNodes
	if next.ApprovalNodes > 0 {
		next.Attention = AttentionApproval
	} else if next.UserInputNodes > 0 {
		next.Attention = AttentionUserInput
	}

	switch {
	case next.WaitingNodes > 0:
		next.LegacyStatus = LegacyPermission
	case root.Runtime == RuntimeActive:
		next.LegacyStatus = LegacyWorking
	case root.Runtime == RuntimeSystemError:
		// V1 preserves the explicit error axis without borrowing the attention
		// color for a condition that does not require user permission.
		next.LegacyStatus = ""
	case workingDescendant:
		next.LegacyStatus = LegacyDelegating
	case root.Runtime == RuntimeIdle:
		next.LegacyStatus = LegacyIdle
	}

	return stampSince(next, prior, now)
}

// PositivelyLive reports whether a non-root node has affirmative evidence that
// it is still part of the current activity interval. Mere structural presence
// is not liveness: providers may retain descendants whose runtime is unknown or
// not loaded and whose lifecycle is also unknown. Terminal lifecycle evidence
// always wins over stale runtime or attention fields.
func PositivelyLive(node Node) bool {
	if node.Lifecycle.Terminal() {
		return false
	}
	if node.Attention == AttentionApproval || node.Attention == AttentionUserInput {
		return true
	}
	if node.Runtime == RuntimeActive || node.Runtime == RuntimeIdle {
		return true
	}
	return node.Lifecycle == LifecyclePending || node.Lifecycle == LifecycleRunning
}

func stampSince(next, prior Summary, now time.Time) Summary {
	if prior.Since.IsZero() || !sameDerivedState(next, prior) {
		next.Since = now
	} else {
		next.Since = prior.Since
	}
	return next
}

func sameDerivedState(left, right Summary) bool {
	return left.Runtime == right.Runtime &&
		left.Attention == right.Attention &&
		left.LegacyStatus == right.LegacyStatus &&
		left.LiveChildren == right.LiveChildren &&
		left.WaitingNodes == right.WaitingNodes &&
		left.ApprovalNodes == right.ApprovalNodes &&
		left.UserInputNodes == right.UserInputNodes &&
		left.ErrorNodes == right.ErrorNodes
}
