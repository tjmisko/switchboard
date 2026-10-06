package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var limitT0 = time.Date(2026, 10, 5, 19, 28, 5, 0, time.UTC)

// limitedSession is a Claude session published working, as projection left
// it, with a usage-limit record. Publication reads the published fields; it
// does not re-resolve them.
func limitedSession(resetsAt *time.Time) *Session {
	since := limitT0.Add(-time.Minute)
	s := &Session{PID: 10, Agent: AgentKindClaude, Claude: &AgentInfo{Status: StatusWorking, StatusSince: since}}
	s.AgentGraph = &AgentGraph{RootID: "sess-1", Summary: AgentGraphSummary{Status: StatusWorking, Since: since}}
	s.UsageLimit = &UsageLimit{ObservedAt: limitT0, ResetsAt: resetsAt, Source: UsageLimitSourceClaudeHook}
	return s
}

func TestProjectPublishedShouldReadLimitedWhenTheLimitIsActive(t *testing.T) {
	sess := limitedSession(timePtr(limitT0.Add(3 * time.Hour)))
	snap := ProjectPublished(Snapshot{Sessions: []Session{*sess}}, limitT0.Add(time.Hour))

	got := snap.Sessions[0]
	if got.Claude.Status != StatusLimited || got.AgentGraph.Summary.Status != StatusLimited {
		t.Fatalf("published status = %q/%q, want limited/limited", got.Claude.Status, got.AgentGraph.Summary.Status)
	}
	if got.Claude.StatusSinceWire == nil || !got.Claude.StatusSinceWire.Equal(limitT0) || !got.AgentGraph.Summary.Since.Equal(limitT0) {
		t.Fatalf("published since = %v/%v, want the limit's evidence time", got.Claude.StatusSinceWire, got.AgentGraph.Summary.Since)
	}
	if got.UsageLimit == nil || got.UsageLimit == sess.UsageLimit {
		t.Fatalf("published usage_limit = %p, want a detached copy of %p", got.UsageLimit, sess.UsageLimit)
	}
}

func TestProjectPublishedShouldDropTheRecordWhenTheResetTimeHasPassed(t *testing.T) {
	resetsAt := limitT0.Add(3 * time.Hour)
	for _, now := range []time.Time{resetsAt, resetsAt.Add(time.Second)} {
		snap := ProjectPublished(Snapshot{Sessions: []Session{*limitedSession(&resetsAt)}}, now)
		got := snap.Sessions[0]
		if got.UsageLimit != nil || got.Claude.Status != StatusWorking {
			t.Fatalf("at %v: usage_limit=%+v status=%q, want nil/working", now, got.UsageLimit, got.Claude.Status)
		}
	}
}

func TestProjectPublishedShouldBoundALimitWithNoResetTimeToAWeek(t *testing.T) {
	inside := ProjectPublished(Snapshot{Sessions: []Session{*limitedSession(nil)}}, limitT0.Add(6*24*time.Hour))
	if inside.Sessions[0].Claude.Status != StatusLimited {
		t.Fatalf("six days in: status = %q, want limited", inside.Sessions[0].Claude.Status)
	}
	outside := ProjectPublished(Snapshot{Sessions: []Session{*limitedSession(nil)}}, limitT0.Add(7*24*time.Hour))
	if outside.Sessions[0].UsageLimit != nil {
		t.Fatalf("a week in: usage_limit = %+v, want dropped", outside.Sessions[0].UsageLimit)
	}
}

func TestProjectPublishedShouldLimitAHerdrOnlySessionThroughItsGraph(t *testing.T) {
	sess := Session{PID: 7, Agent: "pi", AgentGraph: &AgentGraph{RootID: "herdr:t", Summary: AgentGraphSummary{Status: StatusIdle}}}
	sess.UsageLimit = &UsageLimit{ObservedAt: limitT0, Source: UsageLimitSourcePiHook}
	snap := ProjectPublished(Snapshot{Sessions: []Session{sess}}, limitT0)
	if got := snap.Sessions[0].AgentGraph.Summary.Status; got != StatusLimited {
		t.Fatalf("pi graph status = %q, want limited", got)
	}
}

func TestRecordUsageLimitShouldIgnoreEvidenceOlderThanTheRecordHeld(t *testing.T) {
	sess := limitedSession(timePtr(limitT0.Add(time.Hour)))
	if sess.RecordUsageLimit(UsageLimit{ObservedAt: limitT0.Add(-time.Second), ResetsAt: timePtr(limitT0.Add(2 * time.Hour))}) {
		t.Fatal("older evidence replaced the record")
	}
	if !sess.UsageLimit.ResetsAt.Equal(limitT0.Add(time.Hour)) {
		t.Fatalf("resets_at = %v, want the newer record's", sess.UsageLimit.ResetsAt)
	}
}

func TestRecordUsageLimitShouldNotAliasTheCallersResetTime(t *testing.T) {
	var sess Session
	resetsAt := limitT0.Add(time.Hour)
	sess.RecordUsageLimit(UsageLimit{ObservedAt: limitT0, ResetsAt: &resetsAt})
	resetsAt = resetsAt.Add(time.Hour)
	if !sess.UsageLimit.ResetsAt.Equal(limitT0.Add(time.Hour)) {
		t.Fatalf("resets_at followed the caller's variable to %v", sess.UsageLimit.ResetsAt)
	}
}

func TestClearUsageLimitShouldKeepTheRecordWhenActivityPredatesTheLimit(t *testing.T) {
	sess := limitedSession(nil)
	if sess.ClearUsageLimit(limitT0.Add(-time.Millisecond)) || sess.UsageLimit == nil {
		t.Fatal("activity from before the limit cleared it")
	}
	if !sess.ClearUsageLimit(limitT0.Add(time.Second)) || sess.UsageLimit != nil {
		t.Fatal("activity after the limit did not clear it")
	}
	if sess.ClearUsageLimit(limitT0.Add(time.Hour)) {
		t.Fatal("clearing an absent record reported a change")
	}
}

func TestStoreShouldPublishLimitedButKeepTheProviderStatusInternally(t *testing.T) {
	store := New("")
	store.Apply(func(m map[int]*Session) {
		sess := limitedSession(timePtr(time.Now().Add(time.Hour)))
		m[sess.PID] = sess
	})
	if got := store.Snapshot().Sessions[0].Claude.Status; got != StatusWorking {
		t.Fatalf("internal snapshot status = %q, want the FSM's working", got)
	}
	if got := store.PublishedSnapshot().Sessions[0].Claude.Status; got != StatusLimited {
		t.Fatalf("published snapshot status = %q, want limited", got)
	}
	var frame Snapshot
	if err := json.Unmarshal(store.CurrentBroadcast().JSON, &frame); err != nil {
		t.Fatal(err)
	}
	if got := frame.Sessions[0].Claude.Status; got != StatusLimited {
		t.Fatalf("broadcast status = %q, want limited", got)
	}
}

func TestStoreShouldRepublishWhenALimitLapsesWithoutAnyMutation(t *testing.T) {
	store := New("")
	resetsAt := time.Now().Add(30 * time.Millisecond)
	store.Apply(func(m map[int]*Session) {
		sess := limitedSession(&resetsAt)
		m[sess.PID] = sess
	})
	ch, cancel := store.Subscribe()
	defer cancel()
	drain(ch)
	time.Sleep(time.Until(resetsAt) + 5*time.Millisecond)
	store.Apply(func(map[int]*Session) {}) // the reconciler's unconditional tick
	select {
	case b := <-ch:
		var frame Snapshot
		if err := json.Unmarshal(b.JSON, &frame); err != nil {
			t.Fatal(err)
		}
		if got := frame.Sessions[0].Claude.Status; got != StatusWorking {
			t.Fatalf("status after lapse = %q, want working", got)
		}
	case <-time.After(time.Second):
		t.Fatal("a lapsed limit did not republish")
	}
}

func drain(ch <-chan Broadcast) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestLoadShouldRestoreAnActiveLimitOverAnIdleProviderStatus(t *testing.T) {
	resetsAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	sess := limitedSession(&resetsAt)
	sess.Claude.SessionID = "sess-1"
	published := ProjectPublished(Snapshot{SchemaVersion: CurrentSchemaVersion, Sessions: []Session{*sess}}, time.Now())
	path := filepath.Join(t.TempDir(), "state.json")
	body, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	store := New(path)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	internal := store.Snapshot().Sessions[0]
	if internal.Claude.Status == StatusLimited || (internal.AgentGraph != nil && internal.AgentGraph.Summary.Status == StatusLimited) {
		t.Fatalf("hydrated FSM status is limited: %q / %+v", internal.Claude.Status, internal.AgentGraph)
	}
	if internal.UsageLimit == nil || !internal.UsageLimit.ResetsAt.Equal(resetsAt) {
		t.Fatalf("hydrated usage_limit = %+v, want resets_at %v", internal.UsageLimit, resetsAt)
	}
	if got := store.PublishedSnapshot().Sessions[0].Claude.Status; got != StatusLimited {
		t.Fatalf("republished status = %q, want limited", got)
	}
}

func TestLoadShouldDropALimitThatLapsedWhileTheDaemonWasDown(t *testing.T) {
	sess := limitedSession(timePtr(time.Now().Add(-time.Minute)))
	sess.Claude.Status = StatusLimited
	path := filepath.Join(t.TempDir(), "state.json")
	body, err := json.Marshal(Snapshot{SchemaVersion: CurrentSchemaVersion, Sessions: []Session{*sess}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(path)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	got := store.PublishedSnapshot().Sessions[0]
	if got.UsageLimit != nil || got.Claude.Status == StatusLimited {
		t.Fatalf("lapsed limit survived Load: %+v status=%q", got.UsageLimit, got.Claude.Status)
	}
}

func TestRecordUsageLimitShouldRefuseEvidenceTheSessionHasAlreadyOutrun(t *testing.T) {
	var sess Session
	sess.ClearUsageLimit(limitT0.Add(time.Second)) // the next prompt, with no record to clear
	if sess.RecordUsageLimit(UsageLimit{ObservedAt: limitT0}) || sess.UsageLimit != nil {
		t.Fatal("a limit read after newer activity greyed the session")
	}
	if !sess.RecordUsageLimit(UsageLimit{ObservedAt: limitT0.Add(2 * time.Second)}) {
		t.Fatal("a limit newer than the activity was refused")
	}
}

func TestProjectPublishedShouldNeverHideALivePermissionBehindALimit(t *testing.T) {
	sess := limitedSession(timePtr(limitT0.Add(time.Hour)))
	sess.Claude.Status = StatusPermission
	sess.AgentGraph.Summary.Status = StatusPermission
	got := ProjectPublished(Snapshot{Sessions: []Session{*sess}}, limitT0).Sessions[0]
	if got.Claude.Status != StatusPermission || got.AgentGraph.Summary.Status != StatusPermission {
		t.Fatalf("published %q/%q, want the red to win", got.Claude.Status, got.AgentGraph.Summary.Status)
	}
}

func TestProjectPublishedShouldReportLiveSubagentsAsPausedWhenTheSessionIsLimited(t *testing.T) {
	sess := limitedSession(timePtr(limitT0.Add(3 * time.Hour)))
	sess.Claude.InFlightSubagents = 2
	sess.AgentGraph.Summary.Status = StatusDelegating
	sess.AgentGraph.Summary.LiveChildren = 2

	got := ProjectPublished(Snapshot{Sessions: []Session{*sess}}, limitT0.Add(time.Hour)).Sessions[0]

	if got.AgentGraph.Summary.LiveChildren != 0 || got.AgentGraph.Summary.PausedChildren != 2 {
		t.Fatalf("live/paused = %d/%d, want 0/2", got.AgentGraph.Summary.LiveChildren, got.AgentGraph.Summary.PausedChildren)
	}
	if got.Claude.InFlightSubagents != 0 {
		t.Fatalf("in_flight_subagents = %d, want 0 under the limit", got.Claude.InFlightSubagents)
	}
}

func TestProjectPublishedShouldKeepSubagentsLiveWhenTheLimitHasLapsed(t *testing.T) {
	resetsAt := limitT0.Add(time.Hour)
	sess := limitedSession(&resetsAt)
	sess.AgentGraph.Summary.LiveChildren = 2

	got := ProjectPublished(Snapshot{Sessions: []Session{*sess}}, resetsAt).Sessions[0]

	if got.AgentGraph.Summary.LiveChildren != 2 || got.AgentGraph.Summary.PausedChildren != 0 {
		t.Fatalf("live/paused = %d/%d, want 2/0", got.AgentGraph.Summary.LiveChildren, got.AgentGraph.Summary.PausedChildren)
	}
}

func TestHydrateUsageLimitShouldReturnPausedSubagentsToLive(t *testing.T) {
	sess := limitedSession(timePtr(limitT0.Add(3 * time.Hour)))
	sess.AgentGraph.Summary = AgentGraphSummary{Status: StatusLimited, PausedChildren: 2}

	hydrateUsageLimit(sess, limitT0.Add(time.Hour))

	if s := sess.AgentGraph.Summary; s.Status != StatusIdle || s.LiveChildren != 2 || s.PausedChildren != 0 {
		t.Fatalf("hydrated summary = %+v, want idle with 2 live and none paused", s)
	}
}
