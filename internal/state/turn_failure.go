package state

import "time"

// StatusError is the published status of a session whose last turn failed to
// reach or authenticate with its provider. Like StatusLimited it is never
// written by hook or FSM logic: the in-memory status keeps whatever the
// provider FSM decided, and the published copy reads error while
// Session.TurnFailure is set (see ProjectPublished).
const StatusError = "error"

// Turn-failure kinds, recorded on TurnFailure.Kind.
const (
	TurnFailureUnreachable = "unreachable"
	TurnFailureAuth        = "auth"
)

// Turn-failure evidence sources, recorded on TurnFailure.Source.
const (
	TurnFailureSourceClaudeHook     = "claude_stop_failure"
	TurnFailureSourceCodexRollout   = "codex_rollout"
	TurnFailureSourceCodexAppServer = "codex_app_server"
	TurnFailureSourcePiHook         = "pi_hook"
)

// TurnFailure is the evidence that a session's turn ended because the provider
// could not be reached or refused its credentials. Like UsageLimit it is
// orthogonal to the status FSM, and any new activity drops it
// (ClearTurnRecords). It has no deadline: it holds until the session acts again.
type TurnFailure struct {
	ObservedAt time.Time `json:"observed_at"`
	Kind       string    `json:"kind"`
	Source     string    `json:"source,omitempty"`
}

// RecordTurnFailure stores failure evidence on the session. It applies the same
// refusals as RecordUsageLimit: evidence older than the record held, or older
// than the newest activity, is ignored. Recording drops an older usage limit;
// newer evidence wins.
func (s *Session) RecordTurnFailure(f TurnFailure) bool {
	if s.TurnFailure != nil && f.ObservedAt.Before(s.TurnFailure.ObservedAt) {
		return false
	}
	if f.ObservedAt.Before(s.turnActivityAt) {
		return false
	}
	s.TurnFailure = cloneTurnFailure(&f)
	if s.UsageLimit != nil && !s.UsageLimit.ObservedAt.After(f.ObservedAt) {
		s.UsageLimit = nil
	}
	return true
}

func projectTurnFailure(sess *Session, now time.Time) {
	if sess.TurnFailure == nil {
		return
	}
	if !turnFailureOverlays(sess, now) {
		return
	}
	sess.TurnFailure = cloneTurnFailure(sess.TurnFailure)
	since := sess.TurnFailure.ObservedAt
	// A failed turn stops its subagents with it, as a usage limit does.
	for _, info := range []*AgentInfo{sess.Claude, sess.Codex, sess.Pi} {
		if info == nil {
			continue
		}
		info.Status, info.StatusSince = StatusError, since
		info.StatusSinceWire = &since
		info.InFlightSubagents = 0
	}
	if sess.AgentGraph != nil {
		summary := &sess.AgentGraph.Summary
		summary.Status = StatusError
		summary.Since = since
		summary.PausedChildren += summary.LiveChildren
		summary.LiveChildren = 0
	}
}

// turnFailureOverlays reports whether publication reads error over the
// session's status at now. It is the one rule both ProjectPublished and
// ExplainStatus apply.
//
// An active usage limit takes precedence (the two records never outlive each
// other's newer evidence, so this only decides a tie). A published permission is
// newer evidence than the failure: a live dialog means the session is acting.
func turnFailureOverlays(sess *Session, now time.Time) bool {
	if sess.TurnFailure == nil || sess.UsageLimit.ActiveAt(now) {
		return false
	}
	for _, info := range []*AgentInfo{sess.Claude, sess.Codex, sess.Pi} {
		if info != nil && info.Status == StatusPermission {
			return false
		}
	}
	if sess.AgentGraph != nil && sess.AgentGraph.Summary.Status == StatusPermission {
		return false
	}
	return true
}

// hydrateTurnFailure undoes the projection on a session read back from
// state.json: the failed turn has ended, so the status beneath error is idle.
func hydrateTurnFailure(sess *Session) {
	for _, info := range []*AgentInfo{sess.Claude, sess.Codex, sess.Pi} {
		if info != nil && info.Status == StatusError {
			info.Status = StatusIdle
		}
	}
	if sess.AgentGraph == nil {
		return
	}
	summary := &sess.AgentGraph.Summary
	if summary.Status == StatusError {
		summary.Status = StatusIdle
	}
	// hydrateUsageLimit has already restored paused children; this covers a
	// session whose only overlay was the failure.
	summary.LiveChildren += summary.PausedChildren
	summary.PausedChildren = 0
}

func cloneTurnFailure(f *TurnFailure) *TurnFailure {
	if f == nil {
		return nil
	}
	value := *f
	return &value
}
