// Package federation builds the read-only, live view shown by local clients.
// The host-local state.Store remains the only owner of local discovery and
// persistence; remote snapshots are detached copies and disappear as soon as
// their transport disappears.
package federation

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// RemoteSource is the deliberately small surface supplied by remotestate.Manager.
// Subscribe values are wakeups, not ordered revisions: consumers re-read the
// current full Snapshot on every receive. Dropped intermediate wakeups can
// delay a view but cannot make it apply a partial or out-of-order delta.
type RemoteSource interface {
	Snapshot() map[string]state.Snapshot
	Subscribe() (<-chan map[string]state.Snapshot, func())
}

type sessionKey struct {
	host      string
	pid       int
	startedAt time.Time
	source    string
}

// View merges one local Store with zero or more live remote snapshot streams.
// It owns no discovery or durable state.
type View struct {
	local    *state.Store
	remote   RemoteSource
	hostname string

	mu          sync.RWMutex
	publishMu   sync.Mutex
	subscribers map[chan state.Snapshot]struct{}
	remoteFocus sessionKey
	routeReady  func(string, int, time.Time) bool
}

func NewView(local *state.Store, hostname string, remote RemoteSource) (*View, error) {
	if local == nil {
		return nil, errors.New("federation: nil local store")
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return nil, errors.New("federation: empty local hostname")
	}
	return &View{
		local:       local,
		remote:      remote,
		hostname:    hostname,
		subscribers: make(map[chan state.Snapshot]struct{}),
	}, nil
}

func (v *View) Hostname() string { return v.hostname }

// SetRouteReady installs the cheap registry-only candidate check used to mark
// aggregate rows navigable. Action-time local/WM validation remains mandatory.
func (v *View) SetRouteReady(check func(string, int, time.Time) bool) {
	v.mu.Lock()
	v.routeReady = check
	v.mu.Unlock()
}

// Snapshot returns a freshly detached aggregate. Local sessions are stamped
// only on this copy, preserving the frozen host-local state.json shape.
func (v *View) Snapshot() state.Snapshot {
	local := v.local.Snapshot()
	remote := map[string]state.Snapshot(nil)
	if v.remote != nil {
		remote = v.remote.Snapshot()
	}

	v.mu.RLock()
	focused := v.remoteFocus
	routeReady := v.routeReady
	v.mu.RUnlock()
	remoteFocusedLive := false
	if focused.host != "" && focused.host != v.hostname {
		if snapshot, ok := remote[focused.host]; ok {
			for _, session := range snapshot.Sessions {
				if session.PID == focused.pid && focused.startedAt.Equal(session.StartedAt) &&
					routeReady != nil && routeReady(focused.host, session.PID, session.StartedAt) {
					remoteFocusedLive = true
					break
				}
			}
		}
	}

	out := state.Snapshot{
		SchemaVersion: local.SchemaVersion,
		// Cross-host clocks are not causal revisions. Stamp the aggregate
		// observation locally instead of taking a misleading max(remote time).
		UpdatedAt:    time.Now(),
		Capabilities: local.Capabilities,
		Sessions:     make([]state.Session, 0, len(local.Sessions)),
	}
	localNavigate := local.Capabilities == nil || local.Capabilities.Navigate
	for _, session := range local.Sessions {
		session.Hostname = v.hostname
		session.Remote = false
		session.Navigable = localNavigate && !session.Headless
		if remoteFocusedLive {
			session.Focused = false
		}
		out.Sessions = append(out.Sessions, session)
	}

	hosts := make([]string, 0, len(remote))
	for host := range remote {
		// A connection which claims the local hostname would collide with the
		// real local namespace. Ignore it rather than displaying two owners for
		// the same (hostname,pid) pair.
		if host != v.hostname {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		snapshot := remote[host]
		for _, session := range snapshot.Sessions {
			session.Hostname = host
			session.Remote = true
			session.Navigable = !session.Headless && routeReady != nil && routeReady(host, session.PID, session.StartedAt)
			// These locate the remote desktop. They are useful to that host's
			// local clients but must never leak into local sorting or action
			// routing, which is rebuilt from the exact WezTerm binding.
			session.Wezterm = nil
			session.Hyprland = nil
			session.Focused = session.Navigable && remoteFocusedLive && focused.host == host && focused.pid == session.PID &&
				focused.startedAt.Equal(session.StartedAt)
			out.Sessions = append(out.Sessions, session)
		}
	}
	return out
}

// Run republishes complete aggregate snapshots whenever either source changes.
// Snapshot itself reads both sources, so a dropped notification is repaired by
// the next one and no merge state can drift.
func (v *View) Run(ctx context.Context) error {
	return v.run(ctx, nil)
}

// RunReady closes ready only after both upstream subscriptions exist and an
// initial current aggregate has been published.
func (v *View) RunReady(ctx context.Context, ready chan<- struct{}) error {
	return v.run(ctx, ready)
}

func (v *View) run(ctx context.Context, ready chan<- struct{}) error {
	local, cancelLocal := v.local.Subscribe()
	defer cancelLocal()

	var remote <-chan map[string]state.Snapshot
	cancelRemote := func() {}
	if v.remote != nil {
		remote, cancelRemote = v.remote.Subscribe()
	}
	defer cancelRemote()
	v.publish()
	if ready != nil {
		close(ready)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-local:
			if !ok {
				local = nil
				continue
			}
			v.publish()
		case _, ok := <-remote:
			if !ok {
				remote = nil
				continue
			}
			v.publish()
		}
	}
}

// SetRemoteFocus overlays the one remote session whose local pane is actually
// active. Passing an empty host clears the overlay. startedAt is the PID-reuse
// fence: an old binding can never mark a replacement process focused.
func (v *View) SetRemoteFocus(host string, pid int, startedAt time.Time) {
	v.SetRemoteFocusFrom("", host, pid, startedAt)
}

// SetRemoteFocusFrom records which local window/pane observation established
// focus. The source token lets a delayed unfocus from an old window clear only
// its own observation, never a newer focused route.
func (v *View) SetRemoteFocusFrom(source, host string, pid int, startedAt time.Time) {
	next := sessionKey{host: host, pid: pid, startedAt: startedAt, source: source}
	v.mu.Lock()
	changed := v.remoteFocus != next
	v.remoteFocus = next
	v.mu.Unlock()
	if changed {
		v.publish()
	}
}

func (v *View) ClearRemoteFocusFrom(source string) {
	v.mu.Lock()
	changed := v.remoteFocus.host != "" && v.remoteFocus.source == source
	if changed {
		v.remoteFocus = sessionKey{}
	}
	v.mu.Unlock()
	if changed {
		v.publish()
	}
}

// ClearRemoteFocusKey clears an observation only if it still belongs to the
// exact process lifetime whose local route was pruned. A late failed lookup
// cannot erase a newer pane's focus observation.
func (v *View) ClearRemoteFocusKey(host string, pid int, startedAt time.Time) {
	v.mu.Lock()
	changed := v.remoteFocus.host == host && v.remoteFocus.pid == pid && v.remoteFocus.startedAt.Equal(startedAt)
	if changed {
		v.remoteFocus = sessionKey{}
	}
	v.mu.Unlock()
	if changed {
		v.publish()
	}
}

// DropRemoteHost forgets a focus observation at the same edge which drops the
// host's live routes. This is stronger than merely hiding a disconnected row:
// if a remote daemon restarts and happens to preserve the same StartedAt value,
// that old local observation must not spring back to life.
func (v *View) DropRemoteHost(host string) {
	v.mu.Lock()
	changed := v.remoteFocus.host == host
	if changed {
		v.remoteFocus = sessionKey{}
	}
	v.mu.Unlock()
	if changed {
		v.publish()
	}
}

// Refresh republishes the aggregate after route metadata changes without a
// source snapshot mutation (for example, a late OSC pane binding).
func (v *View) Refresh() { v.publish() }

// Subscribe receives full-snapshot wakeups after changes. The caller should
// subscribe, obtain Snapshot for its initial frame, and re-read Snapshot on
// every receive; a value queued before the initial read may be older than it.
func (v *View) Subscribe() (<-chan state.Snapshot, func()) {
	ch := make(chan state.Snapshot, 4)
	v.mu.Lock()
	v.subscribers[ch] = struct{}{}
	v.mu.Unlock()
	return ch, func() {
		v.mu.Lock()
		if _, ok := v.subscribers[ch]; ok {
			delete(v.subscribers, ch)
			close(ch)
		}
		v.mu.Unlock()
	}
}

func (v *View) publish() {
	// Source updates and pane callbacks publish from different goroutines. Keep
	// snapshot+fanout in one order so an older snapshot cannot be delivered
	// after a newer focus/route state and strand a quiet subscriber there.
	v.publishMu.Lock()
	defer v.publishMu.Unlock()
	snapshot := v.Snapshot()
	v.mu.RLock()
	defer v.mu.RUnlock()
	for ch := range v.subscribers {
		select {
		case ch <- snapshot:
		default:
			// Coalesce to the latest complete replacement. Dropping the new
			// frame could leave a quiet subscriber permanently stale when this
			// is the final update (notably, a remote disconnect).
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snapshot:
			default:
			}
		}
	}
}
