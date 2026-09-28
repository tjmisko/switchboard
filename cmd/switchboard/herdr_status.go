package main

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/tjmisko/switchboard/internal/herdr"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
)

// herdrStatusSource is the part of herdr.Watcher the daemon uses: herdr's live
// agent status per pane, each server's focused pane, the servers to follow,
// and a lossy change signal.
type herdrStatusSource interface {
	SetServers(ctx context.Context, sockets []string)
	Status(herdr.PaneKey) (herdr.PaneStatus, bool)
	ActivePane(socket string) string
	Changes() <-chan herdr.PaneKey
}

// herdrSockets returns the API socket of every herdr server that owns a pane in
// a tick's terminal enumeration, sorted.
func herdrSockets(panes map[string]terminal.PaneRef) []string {
	seen := make(map[string]bool)
	for _, pane := range panes {
		if pane.Backend == "herdr" && pane.MuxSocket != "" {
			seen[pane.MuxSocket] = true
		}
	}
	sockets := make([]string, 0, len(seen))
	for socket := range seen {
		sockets = append(sockets, socket)
	}
	slices.Sort(sockets)
	return sockets
}

// applyHerdrPane brings one session's herdr block up to date and records the
// transition when herdr moves its published status. It runs under
// Store.Apply and does no I/O: the watcher answers from memory.
//
// pane is the session's pane from this tick's terminal enumeration, or nil when
// the caller has none (a watcher change, or a tick with no batch enumeration).
// Then the session keeps the pane it was last seen in: a tty briefly missing
// from one enumeration must not strip herdr's authority.
func applyHerdrPane(sess *state.Session, pane *terminal.PaneRef, source herdrStatusSource, sink *history.Sink, now time.Time) {
	paneID, socket := "", ""
	switch {
	case pane != nil && pane.Backend == "herdr":
		paneID, socket = pane.Handle, pane.MuxSocket
	case sess.Herdr != nil:
		paneID, socket = sess.Herdr.PaneID, sess.Herdr.Socket
	default:
		return
	}
	status, live := source.Status(herdr.PaneKey{Socket: socket, PaneID: paneID})
	activePane := source.ActivePane(socket)
	if h := sess.Herdr; h != nil && h.PaneID == paneID && h.Socket == socket &&
		h.Agent == status.Agent && h.Status == status.Status && h.Live == live &&
		h.ActivePaneID == activePane {
		return
	}
	since := status.Since
	if since.IsZero() {
		since = now
	}
	priorSince, _, _ := herdrLaneFacts(sess)
	before, after := sess.SetHerdr(state.HerdrReading{
		PaneID: paneID, Socket: socket, TerminalID: status.TerminalID,
		Agent: status.Agent, Status: status.Status, Live: live, Since: since,
		ActivePaneID: activePane,
	}, now)
	if before == after {
		return
	}
	rule := statustune.RuleHerdrAuthority
	if _, decides := state.HerdrLegacyStatus(status.Status, ""); !live || !decides {
		rule = statustune.RuleHerdrReleased
	}
	_, sessionID, subagents := herdrLaneFacts(sess)
	reason := "herdr status=" + status.Status
	if !live {
		reason = "herdr server not followed"
	}
	sink.Record(history.Event{
		Ts: now, Type: history.EventTransition,
		SessionID: sessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		From: before, To: after, Rule: rule, Reason: reason,
		Subagents: subagents, DurPrevMs: history.HeldMs(priorSince, now),
	})
	from, to := before, after
	if from == "" {
		from = "unknown"
	}
	if to == "" {
		to = "unknown"
	}
	age := time.Duration(0)
	if !priorSince.IsZero() && now.After(priorSince) {
		age = now.Sub(priorSince)
	}
	statustune.Decision{
		PID: sess.PID, Session: shortSessionID(sessionID),
		From: from, To: to, Rule: rule, Reason: reason,
		Subagents: subagents, Age: age,
	}.Log()
}

// herdrLaneFacts returns the published status's start, the session id history
// files it under, and its in-flight subagents. A Claude or Codex session has
// them in its enrichment block; an agent herdr alone observes has only its
// graph, whose root id is its session id.
func herdrLaneFacts(sess *state.Session) (since time.Time, sessionID string, subagents int) {
	if info := sess.Enrichment(); info != nil {
		return info.StatusSince, info.SessionID, info.InFlightSubagents
	}
	if sess.AgentGraph != nil {
		return sess.AgentGraph.Summary.Since, sess.AgentGraph.RootID, 0
	}
	return time.Time{}, "", 0
}

// runHerdrStatus lands herdr's status changes at event speed: each signalled
// pane is re-read from the watcher and applied to the sessions in it. The
// reconcile tick re-applies every session too, so a dropped signal only delays
// an edge by one tick.
//
// When herdr's focus moved, refocus re-derives which session the active window
// shows: a pane switch inside herdr moves no OS window, so no WM event would.
func runHerdrStatus(ctx context.Context, store *state.Store, source herdrStatusSource, sink *history.Sink, refocus func(context.Context)) {
	for {
		var first herdr.PaneKey
		select {
		case <-ctx.Done():
			return
		case first = <-source.Changes():
		}
		keys := map[herdr.PaneKey]bool{first: true}
		// Coalesce a burst (a reconnect signals every pane of its server) into
		// one Apply.
	drain:
		for {
			select {
			case key := <-source.Changes():
				keys[key] = true
			default:
				break drain
			}
		}
		now := time.Now()
		focusMoved := false
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				if sess.Herdr == nil || !keys[herdr.PaneKey{Socket: sess.Herdr.Socket, PaneID: sess.Herdr.PaneID}] {
					continue
				}
				activeBefore := sess.Herdr.ActivePaneID
				applyHerdrPane(sess, nil, source, sink, now)
				if sess.Herdr.ActivePaneID != activeBefore {
					focusMoved = true
				}
			}
		})
		if focusMoved && refocus != nil {
			refocus(ctx)
		}
	}
}

// herdrFocusClaimer is the part of herdr.Watcher a trusted focus request uses.
type herdrFocusClaimer interface {
	herdrStatusSource
	Claim(socket, paneID string) bool
	Resync(ctx context.Context, socket string) error
}

// claimHerdrFocus trusts a focus request for a session in a herdr pane before
// herdr confirms it. herdr's pane.focused event trails the request by up to
// its stream's 100 ms poll, and the window raise that precedes it fires a WM
// focus event within a few milliseconds. Left to wait, that WM event lights
// the chip of herdr's previous pane, and herdr's event then moves the light:
// a visible stutter. Claiming the pane first, and re-deriving focus against
// the window active now, makes every later focus pass agree with the request
// from the start. If the window is already active, the chip moves at once.
//
// It returns the settle func for rpc.SetFocusIntent: a navigation that failed
// re-reads herdr's own account, which re-derives focus through the herdr
// status loop. nil when the target is not in a followed herdr pane.
func claimHerdrFocus(ctx context.Context, store *state.Store, activeWindow func(context.Context) (string, error), source herdrFocusClaimer, sink *history.Sink, target state.Session) func(error) {
	if target.Herdr == nil || target.Herdr.Socket == "" || target.Herdr.PaneID == "" {
		return nil
	}
	socket := target.Herdr.Socket
	if !source.Claim(socket, target.Herdr.PaneID) {
		return nil
	}
	active, activeErr := activeWindow(ctx)
	now := time.Now()
	store.Apply(func(m map[int]*state.Session) {
		for _, sess := range m {
			if sess.Herdr != nil && sess.Herdr.Socket == socket {
				applyHerdrPane(sess, nil, source, sink, now)
			}
		}
		if activeErr == nil {
			applyFocus(m, active, sink, now)
		}
	})
	return func(err error) {
		if err == nil {
			return
		}
		if resyncErr := source.Resync(context.WithoutCancel(ctx), socket); resyncErr != nil {
			log.Printf("herdr focus: resync %s after a failed focus: %v", socket, resyncErr)
		}
	}
}
