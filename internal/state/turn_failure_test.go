package state

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/statusexplain"
)

func failedSession(status string) *Session {
	since := limitT0.Add(-time.Minute)
	s := &Session{PID: 10, Agent: AgentKindClaude, Claude: &AgentInfo{Status: status, StatusSince: since}}
	s.AgentGraph = &AgentGraph{RootID: "sess-1", Summary: AgentGraphSummary{Status: status, Since: since}}
	s.TurnFailure = &TurnFailure{ObservedAt: limitT0, Kind: TurnFailureUnreachable, Source: TurnFailureSourceClaudeHook}
	return s
}

func TestProjectPublishedShouldReadErrorWhenATurnFailedUnreachable(t *testing.T) {
	sess := failedSession(StatusWorking)
	snap := ProjectPublished(Snapshot{Sessions: []Session{*sess}}, limitT0.Add(24*time.Hour))

	got := snap.Sessions[0]
	if got.Claude.Status != StatusError || got.AgentGraph.Summary.Status != StatusError {
		t.Fatalf("published status = %q/%q, want error/error", got.Claude.Status, got.AgentGraph.Summary.Status)
	}
	if got.Claude.StatusSinceWire == nil || !got.Claude.StatusSinceWire.Equal(limitT0) || !got.AgentGraph.Summary.Since.Equal(limitT0) {
		t.Fatalf("published since = %v/%v, want the failure's evidence time", got.Claude.StatusSinceWire, got.AgentGraph.Summary.Since)
	}
	if got.TurnFailure == nil || got.TurnFailure == sess.TurnFailure {
		t.Fatalf("published turn_failure = %p, want a detached copy of %p", got.TurnFailure, sess.TurnFailure)
	}
}

func TestProjectPublishedShouldKeepErrorWhenTheUnderlyingStatusIsDelegatingWithLiveChildren(t *testing.T) {
	sess := failedSession(StatusDelegating)
	sess.AgentGraph.Summary.LiveChildren = 2
	sess.Claude.InFlightSubagents = 2
	snap := ProjectPublished(Snapshot{Sessions: []Session{*sess}}, limitT0)

	got := snap.Sessions[0]
	if got.Claude.Status != StatusError || got.AgentGraph.Summary.Status != StatusError {
		t.Fatalf("published status = %q/%q, want error/error", got.Claude.Status, got.AgentGraph.Summary.Status)
	}
	if got.AgentGraph.Summary.LiveChildren != 0 || got.AgentGraph.Summary.PausedChildren != 2 || got.Claude.InFlightSubagents != 0 {
		t.Fatalf("children live=%d paused=%d inflight=%d, want 0/2/0", got.AgentGraph.Summary.LiveChildren, got.AgentGraph.Summary.PausedChildren, got.Claude.InFlightSubagents)
	}
}

func TestClearTurnRecordsShouldDropTheFailureWhenActivityPostdatesIt(t *testing.T) {
	sess := failedSession(StatusWorking)
	if sess.ClearTurnRecords(limitT0.Add(-time.Millisecond)) || sess.TurnFailure == nil {
		t.Fatal("activity before the failure cleared it")
	}
	if !sess.ClearTurnRecords(limitT0.Add(time.Second)) || sess.TurnFailure != nil {
		t.Fatal("later activity did not clear the failure")
	}
}

func TestRecordTurnFailureShouldRefuseAFailureOlderThanTheNewestActivity(t *testing.T) {
	sess := &Session{PID: 10}
	sess.ClearTurnRecords(limitT0.Add(time.Second))
	if sess.RecordTurnFailure(TurnFailure{ObservedAt: limitT0, Kind: TurnFailureAuth}) || sess.TurnFailure != nil {
		t.Fatalf("recorded a failure older than activity: %+v", sess.TurnFailure)
	}
}

func TestRecordTurnFailureShouldRefuseAFailureOlderThanTheHeldRecord(t *testing.T) {
	sess := failedSession(StatusWorking)
	if sess.RecordTurnFailure(TurnFailure{ObservedAt: limitT0.Add(-time.Second), Kind: TurnFailureAuth}) || sess.TurnFailure.Kind != TurnFailureUnreachable {
		t.Fatalf("an older failure replaced the held one: %+v", sess.TurnFailure)
	}
}

func TestRecordShouldLetNewerEvidenceReplaceTheOlderRecordKind(t *testing.T) {
	sess := limitedSession(nil)
	if !sess.RecordTurnFailure(TurnFailure{ObservedAt: limitT0.Add(time.Second), Kind: TurnFailureUnreachable}) {
		t.Fatal("newer failure refused")
	}
	if sess.UsageLimit != nil || sess.TurnFailure == nil {
		t.Fatalf("limit=%+v failure=%+v, want the failure to replace the limit", sess.UsageLimit, sess.TurnFailure)
	}
	if !sess.RecordUsageLimit(UsageLimit{ObservedAt: limitT0.Add(2 * time.Second)}) {
		t.Fatal("newer limit refused")
	}
	if sess.UsageLimit == nil || sess.TurnFailure != nil {
		t.Fatalf("limit=%+v failure=%+v, want the limit to replace the failure", sess.UsageLimit, sess.TurnFailure)
	}
}

func TestProjectPublishedShouldNotOverlayErrorWhenTheStatusIsPermission(t *testing.T) {
	sess := failedSession(StatusPermission)
	snap := ProjectPublished(Snapshot{Sessions: []Session{*sess}}, limitT0)
	got := snap.Sessions[0]
	if got.Claude.Status != StatusPermission || got.AgentGraph.Summary.Status != StatusPermission {
		t.Fatalf("published status = %q/%q, want permission/permission", got.Claude.Status, got.AgentGraph.Summary.Status)
	}
	if d := sess.ExplainStatus(limitT0); d.Underlying != nil {
		t.Fatalf("explain overlaid a permission: %+v", d.Choice)
	}
}

func TestHydrateTurnFailureShouldReadAPersistedErrorBackToIdle(t *testing.T) {
	sess := failedSession(StatusError)
	sess.AgentGraph.Summary.PausedChildren = 2
	hydrateUsageLimit(sess, limitT0)
	hydrateTurnFailure(sess)
	if sess.Claude.Status != StatusIdle || sess.AgentGraph.Summary.Status != StatusIdle {
		t.Fatalf("hydrated status = %q/%q, want idle/idle", sess.Claude.Status, sess.AgentGraph.Summary.Status)
	}
	if sess.AgentGraph.Summary.LiveChildren != 2 || sess.AgentGraph.Summary.PausedChildren != 0 {
		t.Fatalf("children live=%d paused=%d, want 2/0", sess.AgentGraph.Summary.LiveChildren, sess.AgentGraph.Summary.PausedChildren)
	}
}

func TestExplainStatusShouldNameTheTurnFailureOverlayWhenPublishedError(t *testing.T) {
	sess := failedSession(StatusWorking)
	d := sess.ExplainStatus(limitT0.Add(time.Hour))
	if d.Choice.Status != StatusError || d.Choice.Reason != statusexplain.ReasonTurnFailureOverlay || d.Choice.EvidenceKind != statusexplain.EvidenceTurnFailure {
		t.Fatalf("choice = %+v, want the error overlay", d.Choice)
	}
	if d.Underlying == nil {
		t.Fatal("underlying decision not set")
	}
	if !d.Choice.FreshUntil.IsZero() || !d.Choice.ObservedAt.Equal(limitT0) {
		t.Fatalf("choice times = %v/%v", d.Choice.ObservedAt, d.Choice.FreshUntil)
	}
}
