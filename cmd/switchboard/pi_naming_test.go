package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func named(name string) func(*rpc.Request) {
	return func(req *rpc.Request) { req.SessionName = name }
}

// wantName checks the session's display name, as the bar's label resolves it.
func (h *piHarness) wantName(step, want string) {
	h.t.Helper()
	sess := h.session()
	if want == "" {
		if sess.DisplayName != nil {
			h.t.Fatalf("%s: display name = %+v, want none", step, sess.DisplayName)
		}
		return
	}
	if sess.DisplayName == nil || sess.DisplayName.Origin != state.DisplayNameNative || sess.DisplayName.Value != want {
		h.t.Fatalf("%s: display name = %+v, want native %q", step, sess.DisplayName, want)
	}
	if got := label.RawName(sess); got != want {
		h.t.Fatalf("%s: label = %q, want %q", step, got, want)
	}
}

func TestPiNamingShouldShowPisNameAsTheDisplayNameWhenTheSessionStarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"), named("Refactor auth module"))
	h.wantName("session start", "Refactor auth module")
	if conversation := h.session().DisplayName.ConversationID; conversation != piTestSession {
		t.Fatalf("name bound to %q, want the Pi session %q", conversation, piTestSession)
	}
}

func TestPiNamingShouldUpdateTheDisplayNameWhenPisSessionIsRenamedMidSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"))
	h.hook("UserPromptSubmit", time.Second)
	h.wantName("unnamed", "")
	freshUntil := h.session().AgentGraph.FreshUntil

	h.hook("SessionName", 2*time.Second, named("first name"))
	h.wantName("first rename", "first name")
	h.hook("SessionName", 3*time.Second, named("second name"))
	h.wantName("second rename", "second name")
	// A rename sent earlier but delivered late does not undo the newer one.
	h.hook("SessionName", 2500*time.Millisecond, named("first name"))
	h.wantName("late rename", "second name")

	h.wantStatus("renames carry no status", state.StatusWorking)
	if got := h.session().AgentGraph.FreshUntil; !got.Equal(freshUntil) {
		t.Fatalf("a rename renewed the hook lease: %v, want %v", got, freshUntil)
	}

	h.hook("SessionName", 4*time.Second, named(""))
	h.wantName("cleared", "")
	for _, event := range h.events() {
		if event.Type == history.EventTransition && event.Ts.After(h.base.Add(time.Second)) {
			t.Fatalf("a rename recorded a transition: %+v", event)
		}
	}
}

func TestPiNamingShouldResetTheNameWhenPiMovesToAnotherSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := newPiHarness(t)
	h.hook("SessionStart", 0, source("startup"), named("old work"))
	h.hook("SessionStart", time.Second, source("new"), session(piTestNext))
	h.wantName("after /new", "")
	// A rename still addressed to the session left is not applied to the new one.
	h.hook("SessionName", 2*time.Second, named("stale"))
	h.wantName("rename for the old session", "")
	h.hook("SessionName", 3*time.Second, session(piTestNext), named("new work"))
	h.wantName("rename for the new session", "new work")
	h.hook("SessionStart", 4*time.Second, source("resume"), session(piTestSession), named("old work"))
	h.wantName("after /resume", "old work")
}
