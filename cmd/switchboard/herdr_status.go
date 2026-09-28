package main

import (
	"context"
	"slices"
	"time"

	"github.com/tjmisko/switchboard/internal/herdr"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
)

// herdrStatusSource is the part of herdr.Watcher the daemon uses: herdr's live
// agent status per pane, the servers to follow, and a lossy change signal.
type herdrStatusSource interface {
	SetServers(ctx context.Context, sockets []string)
	Status(herdr.PaneKey) (herdr.PaneStatus, bool)
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
	if h := sess.Herdr; h != nil && h.PaneID == paneID && h.Socket == socket &&
		h.Agent == status.Agent && h.Status == status.Status && h.Live == live {
		return
	}
	since := status.Since
	if since.IsZero() {
		since = now
	}
	var priorSince time.Time
	if info := sess.Enrichment(); info != nil {
		priorSince = info.StatusSince
	}
	before, after := sess.SetHerdr(paneID, socket, status.Agent, status.Status, live, since)
	if before == after {
		return
	}
	rule := statustune.RuleHerdrAuthority
	if _, decides := state.HerdrLegacyStatus(status.Status, ""); !live || !decides {
		rule = statustune.RuleHerdrReleased
	}
	info := sess.Enrichment()
	reason := "herdr status=" + status.Status
	if !live {
		reason = "herdr server not followed"
	}
	sink.Record(history.Event{
		Ts: now, Type: history.EventTransition,
		SessionID: info.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		From: before, To: after, Rule: rule, Reason: reason,
		Subagents: info.InFlightSubagents, DurPrevMs: history.HeldMs(priorSince, now),
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
		PID: sess.PID, Session: shortSessionID(info.SessionID),
		From: from, To: to, Rule: rule, Reason: reason,
		Subagents: info.InFlightSubagents, Age: age,
	}.Log()
}

// runHerdrStatus lands herdr's status changes at event speed: each signalled
// pane is re-read from the watcher and applied to the sessions in it. The
// reconcile tick re-applies every session too, so a dropped signal only delays
// an edge by one tick.
func runHerdrStatus(ctx context.Context, store *state.Store, source herdrStatusSource, sink *history.Sink) {
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
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				if sess.Herdr == nil || !keys[herdr.PaneKey{Socket: sess.Herdr.Socket, PaneID: sess.Herdr.PaneID}] {
					continue
				}
				applyHerdrPane(sess, nil, source, sink, now)
			}
		})
	}
}
