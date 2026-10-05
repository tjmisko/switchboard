package state

import "time"

// StatusLimited is the published status of a session whose provider account has
// hit its usage limit. It is never written by hook or reconciler logic: the
// in-memory status keeps whatever the provider FSM decided, and the published
// copy reads limited while Session.UsageLimit is active (see ProjectPublished).
const StatusLimited = "limited"

// usageLimitUnknownResetHold bounds a limit whose provider gave no reset time.
// The longest window either provider enforces is a week, so a record older than
// that cannot still be the reason the session is blocked.
const usageLimitUnknownResetHold = 7 * 24 * time.Hour

// Usage-limit evidence sources, recorded on UsageLimit.Source.
const (
	UsageLimitSourceClaudeHook   = "claude_stop_failure"
	UsageLimitSourceCodexRollout = "codex_rollout"
	UsageLimitSourcePiHook       = "pi_hook"
)

// UsageLimit is the evidence that a session's turn ended on its provider's usage
// limit. It is orthogonal to the status FSM: hooks and the provider graph keep
// deciding the underlying status, and only publication reads limited over it.
// Any new activity from the session drops the record (ClearUsageLimit), and
// so does reaching ResetsAt.
type UsageLimit struct {
	ObservedAt time.Time  `json:"observed_at"`
	ResetsAt   *time.Time `json:"resets_at,omitempty"`
	Source     string     `json:"source,omitempty"`
}

// ActiveAt reports whether the limit still blocks the session at now. A limit
// with no known reset holds for usageLimitUnknownResetHold past its evidence.
func (u *UsageLimit) ActiveAt(now time.Time) bool {
	if u == nil {
		return false
	}
	if u.ResetsAt != nil {
		return now.Before(*u.ResetsAt)
	}
	return now.Before(u.ObservedAt.Add(usageLimitUnknownResetHold))
}

// RecordUsageLimit stores limit evidence on the session. Evidence older than the
// record already held is ignored, so a late rollout read cannot rewind a newer
// hook's reset time; so is evidence older than the newest activity, so a read
// that loses a race with the next prompt cannot grey a working session.
func (s *Session) RecordUsageLimit(limit UsageLimit) bool {
	if s.UsageLimit != nil && limit.ObservedAt.Before(s.UsageLimit.ObservedAt) {
		return false
	}
	if limit.ObservedAt.Before(s.usageLimitActivityAt) {
		return false
	}
	s.UsageLimit = cloneUsageLimit(&limit)
	return true
}

// ClearUsageLimit notes activity observed at `at` and drops the record when the
// activity postdates its evidence. Activity from before the limit (a reordered
// or delayed hook) cannot clear it. It reports whether a record was dropped.
func (s *Session) ClearUsageLimit(at time.Time) bool {
	if at.After(s.usageLimitActivityAt) {
		s.usageLimitActivityAt = at
	}
	if s.UsageLimit == nil {
		return false
	}
	if !at.IsZero() && at.Before(s.UsageLimit.ObservedAt) {
		return false
	}
	s.UsageLimit = nil
	return true
}

// ProjectPublished returns a detached snapshot as consumers see it: every session
// with an active usage limit reads limited, dated from the limit's evidence, and
// a lapsed record is dropped. It mutates only the snapshot it is given, which
// snapshotLocked has already deep-copied.
func ProjectPublished(snap Snapshot, now time.Time) Snapshot {
	for i := range snap.Sessions {
		projectUsageLimit(&snap.Sessions[i], now)
	}
	return snap
}

func projectUsageLimit(sess *Session, now time.Time) {
	if !sess.UsageLimit.ActiveAt(now) {
		sess.UsageLimit = nil
		return
	}
	// A live red is a decision the user can act on, and a capped turn raises
	// none, so a permission status means the limit no longer holds: the clear
	// that should have said so was lost. Never hide it behind grey.
	if publishedPermission(sess) {
		return
	}
	sess.UsageLimit = cloneUsageLimit(sess.UsageLimit)
	since := sess.UsageLimit.ObservedAt
	for _, info := range []*AgentInfo{sess.Claude, sess.Codex} {
		if info == nil {
			continue
		}
		info.Status, info.StatusSince = StatusLimited, since
		info.StatusSinceWire = &since
	}
	if sess.AgentGraph != nil {
		sess.AgentGraph.Summary.Status = StatusLimited
		sess.AgentGraph.Summary.Since = since
	}
}

func publishedPermission(sess *Session) bool {
	if info := sess.Enrichment(); info != nil {
		return info.Status == StatusPermission
	}
	return sess.AgentGraph != nil && sess.AgentGraph.Summary.Status == StatusPermission
}

// hydrateUsageLimit undoes the projection on a session read back from
// state.json, before its graph is re-projected. The capped turn has ended, so
// the underlying status it stood over is idle. A lapsed record is dropped.
func hydrateUsageLimit(sess *Session, now time.Time) {
	for _, info := range []*AgentInfo{sess.Claude, sess.Codex} {
		if info != nil && info.Status == StatusLimited {
			info.Status = StatusIdle
		}
	}
	if sess.AgentGraph != nil && sess.AgentGraph.Summary.Status == StatusLimited {
		sess.AgentGraph.Summary.Status = StatusIdle
	}
	if !sess.UsageLimit.ActiveAt(now) {
		sess.UsageLimit = nil
	}
}

func cloneUsageLimit(u *UsageLimit) *UsageLimit {
	if u == nil {
		return nil
	}
	value := *u
	if u.ResetsAt != nil {
		resetsAt := *u.ResetsAt
		value.ResetsAt = &resetsAt
	}
	return &value
}
