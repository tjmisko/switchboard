package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

// PaneKey names one pane: its server's API socket and its public pane id. A pane
// that moves to another workspace gets a new id; the watcher re-lists on every
// move, so a key is only ever as stale as the caller's own mapping.
type PaneKey struct {
	Socket string
	PaneID string
}

// PaneStatus is herdr's current reading of the agent in one pane.
type PaneStatus struct {
	TerminalID string    // herdr's id for the pane's terminal; stable across moves
	Agent      string    // detected agent label, "" when none
	Status     string    // StatusWorking, StatusBlocked, StatusDone, StatusIdle or StatusUnknown
	Since      time.Time // when the watcher first saw this status
}

// Watcher follows the agent status of every pane on a set of herdr servers.
//
// herdr has no all-panes status stream: pane.agent_status_changed needs one
// subscription entry per pane, and a request naming a pane that no longer
// exists is rejected whole. So each server runs one loop: list the panes,
// subscribe to the lifecycle events (created, closed, moved, exited, agent
// detected) plus one status entry per pane, list again to cover the gap, then
// stream. Any lifecycle event changes the pane set, so the loop starts over
// with a fresh subscription.
//
// A status counts only while its server is connected, or briefly after it
// drops (Grace), so a restarting herdr does not flash every chip back to the
// provider's status. After that, Status reports nothing and callers fall back.
type Watcher struct {
	call  Caller
	dial  func(ctx context.Context, socket string) (net.Conn, error)
	now   func() time.Time
	grace time.Duration

	changes chan PaneKey

	mu      sync.Mutex
	servers map[string]*watchedServer
}

type watchedServer struct {
	cancel context.CancelFunc
	done   chan struct{}
	panes  map[string]PaneStatus // by pane id
	// activePane is the pane herdr's focus is on, "" when none is known.
	activePane string
	// connected is true while a subscription is streaming; lostAt is when it
	// last stopped, zero before the first connection.
	connected bool
	lostAt    time.Time
}

// DefaultGrace is how long a disconnected server's statuses keep counting.
const DefaultGrace = 10 * time.Second

// NewWatcher returns a watcher over the real sockets.
func NewWatcher() *Watcher {
	var d net.Dialer
	return newWatcher(Call, func(ctx context.Context, socket string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", socket)
	}, time.Now, DefaultGrace)
}

func newWatcher(call Caller, dial func(context.Context, string) (net.Conn, error), now func() time.Time, grace time.Duration) *Watcher {
	return &Watcher{
		call: call, dial: dial, now: now, grace: grace,
		changes: make(chan PaneKey, 256),
		servers: make(map[string]*watchedServer),
	}
}

// Changes signals a pane whose status may have changed. It is lossy by design:
// a full buffer drops signals, and callers re-read every status periodically.
func (w *Watcher) Changes() <-chan PaneKey { return w.changes }

// SetServers makes the watched set exactly sockets: new ones start a loop under
// ctx, and ones no longer listed stop and forget their panes.
func (w *Watcher) SetServers(ctx context.Context, sockets []string) {
	want := make(map[string]bool, len(sockets))
	for _, s := range sockets {
		want[s] = true
	}
	var stopped []*watchedServer
	w.mu.Lock()
	for socket, server := range w.servers {
		if !want[socket] {
			server.cancel()
			stopped = append(stopped, server)
			delete(w.servers, socket)
		}
	}
	for socket := range want {
		if _, ok := w.servers[socket]; ok {
			continue
		}
		serverCtx, cancel := context.WithCancel(ctx)
		server := &watchedServer{cancel: cancel, done: make(chan struct{}), panes: make(map[string]PaneStatus)}
		w.servers[socket] = server
		go func() {
			defer close(server.done)
			w.run(serverCtx, socket)
		}()
	}
	w.mu.Unlock()
	for _, server := range stopped {
		<-server.done
	}
}

// Close stops every loop and waits for them.
func (w *Watcher) Close() { w.SetServers(context.Background(), nil) }

// Status returns herdr's status for the pane while its server's statuses count
// (connected, or within Grace of losing the connection).
func (w *Watcher) Status(key PaneKey) (PaneStatus, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	server, ok := w.servers[key.Socket]
	if !ok || !w.countsLocked(server) {
		return PaneStatus{}, false
	}
	status, ok := server.panes[key.PaneID]
	return status, ok
}

// ActivePane returns the pane herdr's focus is on for the server at socket,
// while its statuses count. It is "" when the server is not followed or reports
// no focused pane; callers then cannot say which of its panes is showing.
func (w *Watcher) ActivePane(socket string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	server, ok := w.servers[socket]
	if !ok || !w.countsLocked(server) {
		return ""
	}
	return server.activePane
}

func (w *Watcher) countsLocked(server *watchedServer) bool {
	if server.connected {
		return true
	}
	return !server.lostAt.IsZero() && w.now().Sub(server.lostAt) < w.grace
}

// errResubscribe ends a stream whose pane set changed; the loop starts over
// at once, without backoff.
var errResubscribe = errors.New("herdr: pane set changed")

const (
	minBackoff = 250 * time.Millisecond
	maxBackoff = 5 * time.Second
)

func (w *Watcher) run(ctx context.Context, socket string) {
	backoff := minBackoff
	for ctx.Err() == nil {
		err := w.stream(ctx, socket)
		w.setConnected(socket, false)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errResubscribe) {
			backoff = minBackoff
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// lifecycleSubscriptions change the pane set or a pane's agent; each one ends
// the stream so the next subscription covers the new set.
var lifecycleSubscriptions = []string{"pane.created", "pane.closed", "pane.moved", "pane.exited", "pane.agent_detected"}

func (w *Watcher) stream(ctx context.Context, socket string) error {
	panes, err := ListPanes(ctx, w.call, socket)
	if err != nil {
		return err
	}
	w.replace(socket, panes)

	conn, err := w.dial(ctx, socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	subscriptions := make([]map[string]string, 0, len(lifecycleSubscriptions)+1+len(panes))
	for _, kind := range lifecycleSubscriptions {
		subscriptions = append(subscriptions, map[string]string{"type": kind})
	}
	subscriptions = append(subscriptions, map[string]string{"type": "pane.focused"})
	for _, p := range panes {
		subscriptions = append(subscriptions, map[string]string{"type": "pane.agent_status_changed", "pane_id": p.PaneID})
	}
	if err := writeRequest(conn, "events.subscribe", map[string]any{"subscriptions": subscriptions}); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	ack, err := reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	if err := DecodeResponse(ack, "events.subscribe", nil); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "pane_not_found" {
			return errResubscribe // a pane closed between the list and the subscribe
		}
		return err
	}

	// Events from before the subscription started are not replayed, so a status
	// that changed between the first list and the ack is only visible to a second
	// read. A pane set that changed in that gap needs a new subscription.
	again, err := ListPanes(ctx, w.call, socket)
	if err != nil {
		return err
	}
	if !samePaneIDs(panes, again) {
		return errResubscribe
	}
	w.replace(socket, again)
	w.setConnected(socket, true)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		var event struct {
			Event string `json:"event"`
			Data  struct {
				PaneID      string  `json:"pane_id"`
				Agent       *string `json:"agent"`
				AgentStatus string  `json:"agent_status"`
			} `json:"data"`
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			continue // one unreadable line is not a reason to drop the stream
		}
		if event.Error != nil {
			return errors.New("herdr subscription: " + event.Error.Code) // e.g. events_lost
		}
		// Status events arrive dotted ("pane.agent_status_changed"), lifecycle
		// events underscored ("pane_closed"); compare one spelling.
		switch strings.ReplaceAll(event.Event, ".", "_") {
		case "pane_agent_status_changed":
			agent := ""
			if event.Data.Agent != nil {
				agent = *event.Data.Agent
			}
			w.update(socket, event.Data.PaneID, agent, event.Data.AgentStatus)
		case "pane_focused":
			w.setActive(socket, event.Data.PaneID)
		case "pane_created", "pane_closed", "pane_moved", "pane_exited", "pane_agent_detected":
			return errResubscribe
		}
	}
}

func samePaneIDs(a, b []Pane) bool {
	if len(a) != len(b) {
		return false
	}
	ids := make(map[string]bool, len(a))
	for _, p := range a {
		ids[p.PaneID] = true
	}
	for _, p := range b {
		if !ids[p.PaneID] {
			return false
		}
	}
	return true
}

// replace makes a server's pane statuses exactly the listed ones.
func (w *Watcher) replace(socket string, panes []Pane) {
	var changed []PaneKey
	w.mu.Lock()
	server, ok := w.servers[socket]
	if !ok {
		w.mu.Unlock()
		return
	}
	listed := make(map[string]bool, len(panes))
	active := ""
	for _, p := range panes {
		listed[p.PaneID] = true
		if p.Focused {
			active = p.PaneID
		}
		if w.setLocked(server, p.PaneID, p.TerminalID, p.AgentName(), p.AgentStatus) {
			changed = append(changed, PaneKey{Socket: socket, PaneID: p.PaneID})
		}
	}
	for id := range server.panes {
		if !listed[id] {
			delete(server.panes, id)
			changed = append(changed, PaneKey{Socket: socket, PaneID: id})
		}
	}
	changed = append(changed, w.setActiveLocked(socket, server, active)...)
	w.mu.Unlock()
	w.signal(changed...)
}

// setActive records the pane herdr's focus moved to.
func (w *Watcher) setActive(socket, paneID string) {
	w.mu.Lock()
	var changed []PaneKey
	if server, ok := w.servers[socket]; ok {
		changed = w.setActiveLocked(socket, server, paneID)
	}
	w.mu.Unlock()
	w.signal(changed...)
}

// setActiveLocked moves a server's active pane. A move changes which of its
// panes is showing, so it returns every pane of the server to signal.
func (w *Watcher) setActiveLocked(socket string, server *watchedServer, paneID string) []PaneKey {
	if server.activePane == paneID {
		return nil
	}
	server.activePane = paneID
	changed := make([]PaneKey, 0, len(server.panes))
	for id := range server.panes {
		changed = append(changed, PaneKey{Socket: socket, PaneID: id})
	}
	return changed
}

func (w *Watcher) update(socket, paneID, agent, status string) {
	w.mu.Lock()
	server, ok := w.servers[socket]
	changed := ok && paneID != "" && w.setLocked(server, paneID, "", agent, status)
	w.mu.Unlock()
	if changed {
		w.signal(PaneKey{Socket: socket, PaneID: paneID})
	}
}

// setLocked records a pane's status. terminalID "" (status events carry none)
// keeps the one the last list gave.
func (w *Watcher) setLocked(server *watchedServer, paneID, terminalID, agent, status string) bool {
	prior, ok := server.panes[paneID]
	if terminalID == "" {
		terminalID = prior.TerminalID
	}
	if ok && prior.TerminalID == terminalID && prior.Agent == agent && prior.Status == status {
		return false
	}
	since := w.now()
	if ok && prior.Status == status {
		since = prior.Since
	}
	server.panes[paneID] = PaneStatus{TerminalID: terminalID, Agent: agent, Status: status, Since: since}
	return true
}

// setConnected records a stream starting or stopping. Either edge can change
// whether every pane of the server counts, so it signals them all.
func (w *Watcher) setConnected(socket string, connected bool) {
	var changed []PaneKey
	w.mu.Lock()
	server, ok := w.servers[socket]
	if ok && server.connected != connected {
		server.connected = connected
		if !connected {
			server.lostAt = w.now()
		}
		for id := range server.panes {
			changed = append(changed, PaneKey{Socket: socket, PaneID: id})
		}
	}
	w.mu.Unlock()
	w.signal(changed...)
}

func (w *Watcher) signal(keys ...PaneKey) {
	for _, key := range keys {
		select {
		case w.changes <- key:
		default: // full: the periodic re-read catches up
		}
	}
}
