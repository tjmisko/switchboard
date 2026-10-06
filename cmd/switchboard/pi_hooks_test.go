package main

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

const (
	piTestPID     = 4900
	piTestSession = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	piTestNext    = "0199b2c3-d4e5-7f60-9a1b-1c2d3e4f5a6b"
)

// piHarness drives Pi hooks through the real RPC server into the coordinator,
// the path a switchboard-ctl pi-hook takes in the daemon.
type piHarness struct {
	t          *testing.T
	store      *state.Store
	sink       *history.Sink
	historyDir string
	c          *agentCoordinator
	encoder    *json.Encoder
	decoder    *json.Decoder
	base       time.Time
	sinkClosed bool
}

func newPiHarness(t *testing.T) *piHarness {
	t.Helper()
	store := state.New("")
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[piTestPID] = &state.Session{
			PID: piTestPID, StartedAt: time.Now().Add(-time.Hour), Agent: state.AgentKindPi, CWD: "/project",
		}
	})
	dir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: dir})
	c := newAgentCoordinator(store, sink, nil, nil)
	server := rpc.New(store, "", terminal.NewNone(), wm.NewNone())
	server.SetAgentHookHandler(c.HandleHook)
	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go server.ServeConnection(ctx, serverSide)
	h := &piHarness{
		t: t, store: store, sink: sink, historyDir: dir, c: c,
		encoder: json.NewEncoder(clientSide), decoder: json.NewDecoder(clientSide),
		base: time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute),
	}
	t.Cleanup(func() {
		cancel()
		clientSide.Close()
		c.Close()
		if !h.sinkClosed {
			sink.Close()
		}
	})
	return h
}

// hook sends one Pi hook that fired offset after the harness's base instant.
func (h *piHarness) hook(event string, offset time.Duration, edit ...func(*rpc.Request)) {
	h.t.Helper()
	req := rpc.Request{
		Cmd: "hook", PID: piTestPID, Agent: state.AgentKindPi, Event: event,
		SessionID: piTestSession, ObservedAt: h.base.Add(offset),
	}
	for _, f := range edit {
		f(&req)
	}
	if err := h.encoder.Encode(req); err != nil {
		h.t.Fatal(err)
	}
	var response rpc.Response
	if err := h.decoder.Decode(&response); err != nil {
		h.t.Fatal(err)
	}
}

func dialogs(n int) func(*rpc.Request) {
	return func(req *rpc.Request) { req.OpenDialogs = &n }
}

func busy(req *rpc.Request) { req.Busy = true }

func source(value string) func(*rpc.Request) {
	return func(req *rpc.Request) { req.HookSource = value }
}

func session(id string) func(*rpc.Request) {
	return func(req *rpc.Request) { req.SessionID = id }
}

func (h *piHarness) session() state.Session {
	h.t.Helper()
	for _, sess := range h.store.Snapshot().Sessions {
		if sess.PID == piTestPID {
			return sess
		}
	}
	h.t.Fatal("pi session gone")
	return state.Session{}
}

// status is the Pi block's published status, "" while it has none.
func (h *piHarness) status() string {
	if info := h.session().Pi; info != nil {
		return info.Status
	}
	return ""
}

func (h *piHarness) wantStatus(step, want string) {
	h.t.Helper()
	if got := h.status(); got != want {
		h.t.Fatalf("%s: status = %q, want %q", step, got, want)
	}
}

func (h *piHarness) events() []history.Event {
	h.t.Helper()
	if !h.sinkClosed {
		h.sink.Close()
		h.sinkClosed = true
	}
	events, err := history.ReadRange(h.historyDir, h.base.Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		h.t.Fatal(err)
	}
	return events
}

func TestPiReducerShouldGoWorkingOnAgentStartAndIdleOnlyOnAgentSettled(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"))
	h.wantStatus("session start", state.StatusIdle)
	h.hook("UserPromptSubmit", time.Second)
	h.wantStatus("agent_start", state.StatusWorking)

	// The extension never forwards agent_end (a retry can follow it). Nothing
	// a run emits on its way to agent_settled may idle it.
	for i, event := range []string{"Usage", "PreToolUse", "PostToolUse", "SessionEnd", "AgentEnd", "Notification"} {
		h.hook(event, time.Duration(2+i)*time.Second)
		h.wantStatus(event, state.StatusWorking)
	}
	h.hook("PermissionResolved", 10*time.Second, dialogs(0))
	h.wantStatus("a dialog count of zero mid-run", state.StatusWorking)

	h.hook("Stop", 11*time.Second)
	h.wantStatus("agent_settled", state.StatusIdle)
}

func TestPiReducerShouldShowRedWhileADialogIsOpenAndReturnToWorkingWhenItClosesMidRun(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	h.hook("PermissionRequest", 2*time.Second, dialogs(1))
	h.wantStatus("dialog open", state.StatusPermission)
	if graph := h.session().AgentGraph; graph.Summary.Attention != agentgraph.AttentionUserInput || graph.Source != agentgraph.SourceHook {
		t.Fatalf("dialog graph = %+v, want hook-sourced user_input attention", graph.Summary)
	}
	h.hook("PostToolUse", 3*time.Second)
	h.wantStatus("tool activity while the dialog is open", state.StatusPermission)
	h.hook("PermissionResolved", 4*time.Second, dialogs(0))
	h.wantStatus("dialog closed mid-run", state.StatusWorking)

	h.hook("Stop", 5*time.Second)
	h.hook("PermissionRequest", 6*time.Second, dialogs(1))
	h.wantStatus("dialog open while idle", state.StatusPermission)
	h.hook("PermissionResolved", 7*time.Second, dialogs(0))
	h.wantStatus("dialog closed while idle", state.StatusIdle)
}

func TestPiReducerShouldStayRedWhileEitherTheUIPromptSpanOrHerdrsBlockedCounterIsOpen(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	// The extension counts its ui_prompt span plus herdr's blocked counter.
	h.hook("PermissionRequest", 2*time.Second, dialogs(1)) // ui span opens
	h.hook("PermissionRequest", 3*time.Second, dialogs(2)) // an approval gate blocks too
	h.hook("PermissionResolved", 4*time.Second, dialogs(1))
	h.wantStatus("ui span closed, herdr counter still open", state.StatusPermission)
	h.hook("PermissionRequest", 5*time.Second, dialogs(2))
	h.hook("PermissionResolved", 6*time.Second, dialogs(1))
	h.wantStatus("herdr counter closed, ui span still open", state.StatusPermission)
	h.hook("PermissionResolved", 7*time.Second, dialogs(0))
	h.wantStatus("both closed", state.StatusWorking)
}

func TestPiReducerShouldIgnoreAStaleDialogCountWhenHooksArriveOutOfOrder(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	// The dialog opened at 2s and closed at 3s, but the close was delivered
	// first: the late open must not latch the chip red.
	h.hook("PermissionResolved", 3*time.Second, dialogs(0))
	h.hook("PermissionRequest", 2*time.Second, dialogs(1))
	h.wantStatus("a late, older dialog count", state.StatusWorking)
	// Nor may any older status edge repaint newer evidence.
	h.hook("Stop", 2500*time.Millisecond)
	h.wantStatus("a late, older Stop", state.StatusWorking)
	if got := h.c.Diagnostics(); len(got) == 0 {
		t.Fatal("no diagnostic recorded the rejected hooks")
	}
	h.hook("Stop", 4*time.Second)
	h.wantStatus("a newer Stop", state.StatusIdle)
}

func TestPiReducerShouldTreatAReloadMidRunAsWorkingWhenPiReportsBusy(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	h.hook("SessionEnd", 2*time.Second, source("reload"))
	h.wantStatus("session end holds", state.StatusWorking)
	h.hook("SessionStart", 3*time.Second, source("reload"), busy)
	h.wantStatus("reload mid-run", state.StatusWorking)
	h.hook("Stop", 4*time.Second)
	h.hook("SessionStart", 5*time.Second, source("reload"))
	h.wantStatus("reload while idle", state.StatusIdle)
	// A reload rebuilds the extension, so the run it reported busy is open
	// until Pi settles it: a dialog closing inside it returns to working.
	h.hook("SessionStart", 6*time.Second, source("reload"), busy)
	h.hook("PermissionRequest", 7*time.Second, dialogs(1))
	h.hook("PermissionResolved", 8*time.Second, dialogs(0))
	h.wantStatus("dialog closed inside the reloaded run", state.StatusWorking)
}

func TestPiReducerShouldRebindTheRootOnNewAndResumeAndNotOnReload(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"), named("work"))
	h.hook("UserPromptSubmit", time.Second)

	h.hook("SessionStart", 2*time.Second, source("reload"), busy, named("work"))
	if sess := h.session(); sess.Pi.SessionID != piTestSession || sess.AgentGraph.RootID != piTestSession || sess.DisplayName == nil {
		t.Fatalf("reload rebound the root: pi=%+v root=%q", sess.Pi, sess.AgentGraph.RootID)
	}

	// /new: Pi reports the file it left.
	h.hook("SessionStart", 3*time.Second, source("new"), session(piTestNext), func(req *rpc.Request) {
		req.Transcript = "/home/u/.pi/agent/sessions/--p--/2026-10-05T21-00-00-000Z_" + piTestNext + ".jsonl"
		req.PreviousSessionFile = "/home/u/.pi/agent/sessions/--p--/2026-10-05T20-00-00-000Z_" + piTestSession + ".jsonl"
	})
	sess := h.session()
	if sess.Pi.SessionID != piTestNext || sess.AgentGraph.RootID != piTestNext {
		t.Fatalf("/new: pi=%+v root=%q, want both at the new session", sess.Pi, sess.AgentGraph.RootID)
	}
	if sess.DisplayName != nil {
		t.Fatalf("/new kept the old conversation's display name: %+v", sess.DisplayName)
	}
	h.wantStatus("/new", state.StatusIdle)

	// /resume back to the first session.
	h.hook("SessionStart", 4*time.Second, source("resume"), session(piTestSession))
	if sess := h.session(); sess.Pi.SessionID != piTestSession || sess.AgentGraph.RootID != piTestSession {
		t.Fatalf("/resume: pi=%+v root=%q", sess.Pi, sess.AgentGraph.RootID)
	}

	var starts []history.Event
	for _, event := range h.events() {
		switch event.Type {
		case history.EventSessionEnd:
			t.Fatalf("a rotation wrote session_end for %q; the process did not die", event.SessionID)
		case history.EventSessionStart:
			starts = append(starts, event)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("lanes: %d session_starts, want one per rotation and none for the reload", len(starts))
	}
	if starts[0].SessionID != piTestNext || starts[0].PrevSessionID != piTestSession {
		t.Fatalf("/new lane = start %q prev %q", starts[0].SessionID, starts[0].PrevSessionID)
	}
	if starts[1].SessionID != piTestSession || starts[1].PrevSessionID != piTestNext {
		t.Fatalf("/resume lane = start %q prev %q", starts[1].SessionID, starts[1].PrevSessionID)
	}

	// Replayed, each rotation ends one lane and opens the next on one pid.
	var got []string
	for _, lane := range history.BuildSwimlanes(h.events(), time.Now().Add(time.Hour)) {
		if lane.PID == piTestPID {
			got = append(got, lane.SessionID)
		}
	}
	if want := []string{piTestSession, piTestNext, piTestSession}; !slices.Equal(got, want) {
		t.Fatalf("replayed lanes = %q, want %q", got, want)
	}
}

func TestPiReducerShouldPairARotationByPreviousSessionFileWhenItNamesTheSessionLeft(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	const other = "0199c3d4-e5f6-7a8b-9c0d-2e3f4a5b6c7d"
	h.hook("SessionStart", time.Second, source("fork"), session(piTestNext), func(req *rpc.Request) {
		req.PreviousSessionFile = "/s/2026-10-05T20-00-00-000Z_" + other + ".jsonl"
	})
	for _, event := range h.events() {
		if event.Type == history.EventSessionStart {
			if event.PrevSessionID != other {
				t.Fatalf("prev_session_id = %q, want the id previous_session_file names", event.PrevSessionID)
			}
			return
		}
	}
	t.Fatal("no session_start recorded for the fork")
}

func TestPiReducerShouldRecordTheUsageLimitOnStopFailureAndClearItOnTheNextAgentStart(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	h.hook("StopFailure", 2*time.Second, func(req *rpc.Request) { req.UsageLimit = true })
	h.wantStatus("stop failure", state.StatusIdle)
	if published := h.store.PublishedSnapshot().Sessions[0]; published.Pi.Status != state.StatusLimited || published.UsageLimit == nil {
		t.Fatalf("published %q %+v, want limited", published.Pi.Status, published.UsageLimit)
	}
	h.hook("UserPromptSubmit", 3*time.Second)
	h.wantStatus("next agent_start", state.StatusWorking)
	if published := h.store.PublishedSnapshot().Sessions[0]; published.UsageLimit != nil || published.Pi.Status != state.StatusWorking {
		t.Fatalf("published %q %+v, want the limit cleared", published.Pi.Status, published.UsageLimit)
	}
}

func TestPiReducerShouldRecordEveryEdgeAsATransitionWithAPiRule(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	h.hook("PermissionRequest", 2*time.Second, dialogs(1))
	h.hook("PermissionResolved", 3*time.Second, dialogs(0))
	h.hook("Stop", 4*time.Second)
	want := []struct{ from, to, rule string }{
		{"", state.StatusIdle, statustune.RulePiSessionStarted},
		{state.StatusIdle, state.StatusWorking, statustune.RulePiRunStarted},
		{state.StatusWorking, state.StatusPermission, statustune.RulePiDialogOpen},
		{state.StatusPermission, state.StatusWorking, statustune.RulePiDialogClosed},
		{state.StatusWorking, state.StatusIdle, statustune.RulePiRunSettled},
	}
	var got []history.Event
	for _, event := range h.events() {
		if event.Type == history.EventTransition {
			got = append(got, event)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("transitions = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		if got[i].From != w.from || got[i].To != w.to || got[i].Rule != w.rule || got[i].SessionID != piTestSession || got[i].Agent != state.AgentKindPi {
			t.Errorf("transition %d = %s %s→%s rule=%s, want %s→%s rule=%s", i, got[i].SessionID, got[i].From, got[i].To, got[i].Rule, w.from, w.to, w.rule)
		}
	}
}

func TestPiReducerShouldIgnoreAHookWhenTheSessionIsNotPi(t *testing.T) {
	h := newPiHarness(t)
	h.store.Apply(func(sessions map[int]*state.Session) { sessions[piTestPID].Agent = state.AgentKindClaude })
	h.hook("UserPromptSubmit", 0)
	if sess := h.session(); sess.Pi != nil || sess.AgentGraph != nil {
		t.Fatalf("a pi hook wrote a claude session: pi=%+v graph=%+v", sess.Pi, sess.AgentGraph)
	}
}

func TestPiReducerShouldApplyADialogHookWithoutASessionIDToTheBoundRoot(t *testing.T) {
	h := newPiHarness(t)
	h.hook("PermissionRequest", 0, session(""), dialogs(1))
	if h.session().Pi != nil {
		t.Fatal("an unbound hook with no session id bound a root")
	}
	h.hook("SessionStart", time.Second)
	h.hook("PermissionRequest", 2*time.Second, session(""), dialogs(1))
	h.wantStatus("herdr:blocked carries no session context", state.StatusPermission)
}

func TestPiReducerShouldHandHerdrTheStatusWhenTheHookLeaseLapses(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.hook("UserPromptSubmit", time.Second)
	h.store.Apply(func(sessions map[int]*state.Session) {
		sessions[piTestPID].Herdr = &state.HerdrInfo{PaneID: "w1:p1", Socket: "/s", Status: state.HerdrIdle, Live: true, StatusSince: h.base}
	})
	h.c.reconcilePiRoots(h.base.Add(time.Second + piHookActiveLease - time.Millisecond))
	h.wantStatus("inside the working lease", state.StatusWorking)
	h.c.reconcilePiRoots(h.base.Add(time.Second + piHookActiveLease))
	h.wantStatus("lease lapsed, herdr live", state.StatusIdle)
	var lapsed bool
	for _, event := range h.events() {
		lapsed = lapsed || (event.Type == history.EventTransition && event.Rule == statustune.RulePiHookLapsed)
	}
	if !lapsed {
		t.Fatal("the lapse was not recorded as a transition")
	}
}

func TestPiReducerShouldForgetARootWhenItsProcessIsNoLongerTracked(t *testing.T) {
	h := newPiHarness(t)
	h.hook("SessionStart", 0)
	h.store.Apply(func(sessions map[int]*state.Session) { delete(sessions, piTestPID) })
	h.c.reconcilePiRoots(time.Now())
	h.c.piMu.Lock()
	defer h.c.piMu.Unlock()
	if len(h.c.piRoots) != 0 {
		t.Fatalf("reducer kept %d roots for a gone process", len(h.c.piRoots))
	}
}

func TestPiSessionIDFromFileShouldReadOnlyAPiSessionFileName(t *testing.T) {
	cases := map[string]string{
		"/s/2026-10-05T20-00-00-000Z_" + piTestSession + ".jsonl": piTestSession,
		"/s/" + piTestSession + ".jsonl":                          "",
		"/s/2026_" + piTestSession + ".json":                      "",
		"/s/2026_../etc.jsonl":                                    "",
		"/s/2026_.jsonl":                                          "",
		"":                                                        "",
	}
	for path, want := range cases {
		if got := piSessionIDFromFile(path); got != want {
			t.Errorf("piSessionIDFromFile(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPiReducerShouldRecordTheBindEdgeFromHerdrsStatusWhenAHookFirstBindsAHerdrSession(t *testing.T) {
	h := newPiHarness(t)
	h.store.Apply(func(sessions map[int]*state.Session) {
		sessions[piTestPID].SetHerdr(state.HerdrReading{
			PaneID: "w1:p1", Socket: "/s", TerminalID: "term_1", Agent: state.AgentKindPi,
			Status: state.HerdrWorking, Live: true, Since: h.base,
		}, h.base)
	})
	h.hook("SessionStart", time.Second, source("startup"), busy)
	if sess := h.session(); sess.AgentGraph.RootID != piTestSession || sess.Pi.Status != state.StatusWorking {
		t.Fatalf("bind: root %q status %q, want Pi's session id, working", sess.AgentGraph.RootID, sess.Pi.Status)
	}
	for _, event := range h.events() {
		if event.Type == history.EventTransition {
			t.Fatalf("binding at herdr's own status recorded a transition %s→%s", event.From, event.To)
		}
	}
}
