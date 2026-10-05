package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const piSessionID = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"

// piReading is a herdr reading of pane w1:p1 running pi.
func piReading(status string, since time.Time) HerdrReading {
	r := reading(status, true, since)
	r.Agent = AgentKindPi
	return r
}

func TestPiBlockShouldRoundTripUnderItsOwnKeyWhenPresent(t *testing.T) {
	sess := Session{PID: 20, Agent: AgentKindPi, Pi: &AgentInfo{
		SessionID: piSessionID, Transcript: "/home/u/.pi/agent/sessions/--p--/s.jsonl", Status: StatusWorking,
	}}
	wire, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"pi":{"session_id":"`+piSessionID+`"`) {
		t.Fatalf("wire = %s, want a pi block", wire)
	}
	if strings.Contains(string(wire), `"claude"`) || strings.Contains(string(wire), `"codex"`) {
		t.Fatalf("wire = %s, want no other enrichment block", wire)
	}
	var back Session
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	if back.Pi == nil || back.Pi.SessionID != sess.Pi.SessionID || back.Pi.Transcript != sess.Pi.Transcript || back.Pi.Status != sess.Pi.Status {
		t.Fatalf("round trip = %+v, want %+v", back.Pi, sess.Pi)
	}
}

func TestPiBlockShouldBeOmittedWhenNoPiHookHasBoundTheSession(t *testing.T) {
	for _, sess := range []Session{
		{PID: 20, Agent: AgentKindPi},
		{PID: 21, Agent: AgentKindClaude, Claude: &AgentInfo{Status: StatusIdle}},
	} {
		wire, err := json.Marshal(sess)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), `"pi":`) {
			t.Fatalf("wire = %s, want no pi key", wire)
		}
	}
}

func TestEnrichmentShouldReturnThePiBlockWhenTheSessionIsPi(t *testing.T) {
	pi := &AgentInfo{SessionID: piSessionID, Status: StatusIdle}
	sess := Session{Agent: AgentKindPi, Pi: pi}
	if got := sess.Enrichment(); got != pi {
		t.Fatalf("Enrichment = %+v, want the pi block", got)
	}
	if got := (Session{Agent: AgentKindPi}).Enrichment(); got != nil {
		t.Fatalf("unbound pi Enrichment = %+v, want nil", got)
	}
	if got := (Session{Agent: AgentKindClaude, Pi: pi}).Enrichment(); got != nil {
		t.Fatalf("claude Enrichment = %+v, want nil rather than a stray pi block", got)
	}
}

func TestAgentBlockShouldAllocateOnlyThePiBlockWhenAPiHookArrives(t *testing.T) {
	var sess Session
	block := sess.AgentBlock(AgentKindPi)
	if block == nil || block != sess.Pi {
		t.Fatalf("AgentBlock(pi) = %p, want the session's pi block %p", block, sess.Pi)
	}
	if sess.Agent != AgentKindPi || sess.Claude != nil || sess.Codex != nil {
		t.Fatalf("session = %+v, want agent pi and no other block", sess)
	}
	if again := sess.AgentBlock(AgentKindPi); again != block {
		t.Fatal("AgentBlock(pi) reallocated an existing block")
	}
}

func TestPiRootIDShouldBePiSessionIDWhenAHookHasBoundIt(t *testing.T) {
	if got := PiRootID(piSessionID, "herdr:term_1"); got != piSessionID {
		t.Fatalf("bound root = %q, want %q", got, piSessionID)
	}
	if got := PiRootID("", "herdr:term_1"); got != "herdr:term_1" {
		t.Fatalf("unbound root = %q, want the herdr root", got)
	}
}

func TestSetHerdrShouldRootAPiGraphAtTheHerdrTerminalWhenNoHookHasBoundIt(t *testing.T) {
	s := &Session{PID: 20, Agent: AgentKindPi}
	s.SetHerdr(piReading(HerdrWorking, herdrT0), herdrT0)
	if s.AgentGraph == nil || s.AgentGraph.RootID != "herdr:term_1" {
		t.Fatalf("graph = %+v, want root herdr:term_1", s.AgentGraph)
	}
	if s.Pi != nil {
		t.Fatalf("a herdr reading allocated a pi block: %+v", s.Pi)
	}
}

func TestSetHerdrShouldRootAPiGraphAtPiSessionIDAndProjectIntoItsBlockWhenAHookHasBoundIt(t *testing.T) {
	s := &Session{PID: 20, Agent: AgentKindPi}
	s.SetHerdr(piReading(HerdrIdle, herdrT0), herdrT0)
	s.AgentBlock(AgentKindPi).SessionID = piSessionID // what a Pi hook will bind

	before, after := s.SetHerdr(piReading(HerdrBlocked, herdrT0.Add(time.Second)), herdrT0.Add(time.Second))
	if s.AgentGraph.RootID != piSessionID {
		t.Fatalf("root = %q, want Pi's session id", s.AgentGraph.RootID)
	}
	if before != "" || after != StatusPermission || s.Pi.Status != StatusPermission {
		t.Fatalf("SetHerdr = %q → %q, pi status %q; want the bound block to follow herdr to permission", before, after, s.Pi.Status)
	}
	if !s.Pi.StatusSince.Equal(herdrT0.Add(time.Second)) {
		t.Fatalf("pi status since = %v, want herdr's", s.Pi.StatusSince)
	}
}

func TestStoreSnapshotShouldPublishAPrivateCopyOfThePiBlock(t *testing.T) {
	store := New("")
	store.Apply(func(m map[int]*Session) {
		m[20] = &Session{PID: 20, Agent: AgentKindPi, Pi: &AgentInfo{SessionID: piSessionID, Status: StatusWorking, StatusSince: herdrT0}}
	})
	snap := store.Snapshot()
	pi := snap.Sessions[0].Pi
	if pi == nil || pi.StatusSinceWire == nil || !pi.StatusSinceWire.Equal(herdrT0) {
		t.Fatalf("snapshot pi = %+v, want status_since projected", pi)
	}
	pi.Status = StatusIdle
	if again := store.Snapshot().Sessions[0].Pi; again.Status != StatusWorking {
		t.Fatalf("a snapshot write reached the store: %q", again.Status)
	}
}

func TestProjectPublishedShouldLimitThePiBlockWhenAPiSessionHitsItsLimit(t *testing.T) {
	sess := Session{PID: 20, Agent: AgentKindPi, Pi: &AgentInfo{SessionID: piSessionID, Status: StatusIdle}}
	sess.UsageLimit = &UsageLimit{ObservedAt: limitT0, Source: UsageLimitSourcePiHook}
	snap := ProjectPublished(Snapshot{Sessions: []Session{sess}}, limitT0)
	if got := snap.Sessions[0].Pi.Status; got != StatusLimited {
		t.Fatalf("pi status = %q, want limited", got)
	}
}

func TestLoadShouldRestoreOnlyAPiBlocksDisplayStateWhenTheDaemonRestarts(t *testing.T) {
	since := herdrT0
	persisted := Snapshot{SchemaVersion: CurrentSchemaVersion, Sessions: []Session{{
		PID: 20, Agent: AgentKindPi, TTY: "/dev/pts/4",
		Pi: &AgentInfo{
			SessionID: piSessionID, Transcript: "/home/u/.pi/agent/sessions/--p--/s.jsonl", Status: StatusPermission,
			StatusSinceWire: &since, InFlightSubagents: 2,
			PendingWriters: []string{"main"},
			PendingPrompts: []PendingPromptRecord{{Writer: "main", Tool: "bash", Since: since}},
			Workflows:      []WorkflowStatus{{}},
		},
	}}}
	body, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	pi := store.Snapshot().Sessions[0].Pi
	if pi == nil {
		t.Fatal("pi block was not restored")
	}
	want := AgentInfo{SessionID: piSessionID, Transcript: "/home/u/.pi/agent/sessions/--p--/s.jsonl", Status: StatusPermission}
	if pi.SessionID != want.SessionID || pi.Transcript != want.Transcript || pi.Status != want.Status {
		t.Fatalf("restored pi = %+v, want display state %+v", pi, want)
	}
	if pi.StatusSinceWire != nil || pi.InFlightSubagents != 0 || len(pi.PendingWriters) != 0 ||
		len(pi.PendingPrompts) != 0 || len(pi.Workflows) != 0 {
		t.Fatalf("restored pi = %+v, want no date, prompt ownership, subagents or workflows", pi)
	}
}

func TestLoadShouldRestoreALimitedPiBlockAsIdleWhenTheDaemonRestarts(t *testing.T) {
	persisted := Snapshot{SchemaVersion: CurrentSchemaVersion, Sessions: []Session{{
		PID: 20, Agent: AgentKindPi, Pi: &AgentInfo{SessionID: piSessionID, Status: StatusLimited},
	}}}
	body, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := store.Snapshot().Sessions[0].Pi.Status; got != StatusIdle {
		t.Fatalf("restored pi status = %q, want idle under the lapsed limit", got)
	}
}
