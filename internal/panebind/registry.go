package panebind

import "sync"

// Registry is the in-memory bidirectional route table. New bindings are
// candidates independent of liveness; only Resolve authorizes navigation, and
// only for an exact session present in the latest complete live set for its
// host. Once a candidate has been observed live, a later complete replacement
// which omits it prunes that dead route.
type Registry struct {
	mu sync.RWMutex

	bySession map[ExactSessionKey]map[paneIdentity]LocalPaneRef
	byPane    map[paneIdentity]paneBinding
	liveHost  map[string]map[ExactSessionKey]struct{}
}

// paneIdentity excludes WindowID because WezTerm preserves a pane ID when the
// pane moves to another tab/window. WindowID is refreshable route metadata.
type paneIdentity struct {
	GUIPID int
	PaneID int
}

type paneBinding struct {
	Session  ExactSessionKey
	Ref      LocalPaneRef
	SeenLive bool
}

func identityOf(p LocalPaneRef) paneIdentity {
	return paneIdentity{GUIPID: p.GUIPID, PaneID: p.PaneID}
}

func NewRegistry() *Registry {
	return &Registry{
		bySession: make(map[ExactSessionKey]map[paneIdentity]LocalPaneRef),
		byPane:    make(map[paneIdentity]paneBinding),
		liveHost:  make(map[string]map[ExactSessionKey]struct{}),
	}
}

// Bind replaces any prior session binding for pane. It is the compatibility
// wrapper for callers which do not need to clear metadata owned by the prior
// exact session.
func (r *Registry) Bind(key ExactSessionKey, pane LocalPaneRef) error {
	_, err := r.BindReplacing(key, pane)
	return err
}

// BindingReplacement describes the prior owner observed atomically with a
// BindReplacing update. Changed is false only for an exact key+ref replay.
type BindingReplacement struct {
	PreviousSession ExactSessionKey
	PreviousRef     LocalPaneRef
	HadPrevious     bool
	Changed         bool
}

// BindReplacing installs key and atomically returns the binding which
// previously owned the stable (GUI PID,pane ID), if any. Callers use Changed
// to invalidate focus for first/new/key/ref-changing binds without clearing a
// newer pane-state after a delayed exact idempotent re-announcement.
//
// Like Bind, it deliberately succeeds before the first remote snapshot; the
// candidate remains non-navigable until ReplaceLive supplies the matching
// exact session.
func (r *Registry) BindReplacing(key ExactSessionKey, pane LocalPaneRef) (BindingReplacement, error) {
	if err := key.Validate(); err != nil {
		return BindingReplacement{}, err
	}
	if err := pane.Validate(); err != nil {
		return BindingReplacement{}, err
	}
	key = key.Canonical()
	id := identityOf(pane)
	r.mu.Lock()
	defer r.mu.Unlock()
	result := BindingReplacement{Changed: true}
	seenLive := r.isLiveLocked(key)
	if old, ok := r.byPane[id]; ok {
		result.PreviousSession = old.Session
		result.PreviousRef = old.Ref
		result.HadPrevious = true
		result.Changed = old.Session != key || old.Ref != pane
		if old.Session == key && old.SeenLive {
			seenLive = true
		}
		if old.Session != key {
			delete(r.bySession[old.Session], id)
			if len(r.bySession[old.Session]) == 0 {
				delete(r.bySession, old.Session)
			}
		}
	}
	panes := r.bySession[key]
	if panes == nil {
		panes = make(map[paneIdentity]LocalPaneRef)
		r.bySession[key] = panes
	}
	panes[id] = pane
	r.byPane[id] = paneBinding{Session: key, Ref: pane, SeenLive: seenLive}
	return result, nil
}

func (r *Registry) UnbindPane(pane LocalPaneRef) {
	id := identityOf(pane)
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.byPane[id]
	if !ok {
		return
	}
	delete(r.byPane, id)
	delete(r.bySession[binding.Session], id)
	if len(r.bySession[binding.Session]) == 0 {
		delete(r.bySession, binding.Session)
	}
}

// UnbindPaneSession removes the stable pane identity only while it is still
// owned by key. WindowID is intentionally ignored: local-session cleanup can
// observe a moved pane through a stale/non-live candidate, while a newer bind
// to another exact session must survive the compare-and-delete.
func (r *Registry) UnbindPaneSession(key ExactSessionKey, pane LocalPaneRef) bool {
	if key.Validate() != nil || pane.Validate() != nil {
		return false
	}
	key = key.Canonical()
	id := identityOf(pane)
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.byPane[id]
	if !ok || binding.Session != key {
		return false
	}
	delete(r.byPane, id)
	delete(r.bySession[key], id)
	if len(r.bySession[key]) == 0 {
		delete(r.bySession, key)
	}
	return true
}

// UnbindRoute removes pane only when it is still the exact current route for
// key. It is intended for action-time self-healing after a definitive local
// missing-pane/socket result: a concurrent pane move or session rebind makes
// the comparison fail and preserves the newer candidate.
func (r *Registry) UnbindRoute(key ExactSessionKey, pane LocalPaneRef) bool {
	if key.Validate() != nil || pane.Validate() != nil {
		return false
	}
	key = key.Canonical()
	id := identityOf(pane)
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.byPane[id]
	if !ok || binding.Session != key || binding.Ref != pane {
		return false
	}
	delete(r.byPane, id)
	delete(r.bySession[key], id)
	if len(r.bySession[key]) == 0 {
		delete(r.bySession, key)
	}
	return true
}

// ReplaceLive atomically replaces one hostname's complete live set. A PID may
// occur at most once because (hostname,pid) is the live row namespace. A
// candidate is retained through snapshots until its exact key has appeared at
// least once; after that, omission is definitive and prunes it.
func (r *Registry) ReplaceLive(host string, sessions []ExactSessionKey) error {
	if err := validateHostname(host); err != nil {
		return err
	}
	next := make(map[ExactSessionKey]struct{}, len(sessions))
	seenPID := make(map[int]struct{}, len(sessions))
	for _, key := range sessions {
		if err := key.Validate(); err != nil || key.Hostname != host {
			return ErrInvalidSession
		}
		if _, exists := seenPID[key.PID]; exists {
			return ErrDuplicateLivePID
		}
		seenPID[key.PID] = struct{}{}
		next[key.Canonical()] = struct{}{}
	}
	r.mu.Lock()
	for id, binding := range r.byPane {
		if binding.Session.Hostname != host {
			continue
		}
		if _, live := next[binding.Session]; live {
			binding.SeenLive = true
			r.byPane[id] = binding
			continue
		}
		if binding.SeenLive {
			r.deleteBindingLocked(id, binding)
		}
	}
	r.liveHost[host] = next
	r.mu.Unlock()
	return nil
}

// DropLiveHost makes every route for host immediately non-navigable and drops
// its candidates. A remote daemon restart can preserve a discovery timestamp
// across downtime, so reconnect must require a fresh terminal announcement
// rather than accidentally reauthorizing an old pane.
func (r *Registry) DropLiveHost(host string) {
	r.mu.Lock()
	for id, binding := range r.byPane {
		if binding.Session.Hostname != host {
			continue
		}
		r.deleteBindingLocked(id, binding)
	}
	delete(r.liveHost, host)
	r.mu.Unlock()
}

func (r *Registry) deleteBindingLocked(id paneIdentity, binding paneBinding) {
	delete(r.byPane, id)
	delete(r.bySession[binding.Session], id)
	if len(r.bySession[binding.Session]) == 0 {
		delete(r.bySession, binding.Session)
	}
}

// Resolve returns the sole candidate pane only if the exact session is live.
// More than one pane fails closed instead of guessing which client is intended.
func (r *Registry) Resolve(key ExactSessionKey) (LocalPaneRef, error) {
	if err := key.Validate(); err != nil {
		return LocalPaneRef{}, err
	}
	key = key.Canonical()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.isLiveLocked(key) {
		return LocalPaneRef{}, ErrSessionNotLive
	}
	panes := r.bySession[key]
	switch len(panes) {
	case 0:
		return LocalPaneRef{}, ErrSessionUnbound
	case 1:
		for _, pane := range panes {
			return pane, nil
		}
	}
	return LocalPaneRef{}, ErrSessionAmbiguous
}

// SessionForPane performs the inverse lookup used by pane-state callbacks. The
// stable join is (GUI PID,pane ID); when the pane moved, its locally reported
// WindowID refreshes the same binding. live is false for a pre-snapshot
// candidate or a disconnected/stale session.
func (r *Registry) SessionForPane(pane LocalPaneRef) (key ExactSessionKey, bound, live bool) {
	if pane.Validate() != nil {
		return ExactSessionKey{}, false, false
	}
	id := identityOf(pane)
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, bound := r.byPane[id]
	if !bound {
		return ExactSessionKey{}, false, false
	}
	key = binding.Session
	live = r.isLiveLocked(key)
	if live && binding.Ref != pane {
		binding.Ref = pane
		r.byPane[id] = binding
		r.bySession[binding.Session][id] = pane
	}
	return key, true, live
}

// RefreshLiveRoute conditionally updates a moved pane's current WindowID. It
// never overwrites a concurrent rebind to another session.
func (r *Registry) RefreshLiveRoute(key ExactSessionKey, pane LocalPaneRef) bool {
	if key.Validate() != nil || pane.Validate() != nil {
		return false
	}
	key = key.Canonical()
	id := identityOf(pane)
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.byPane[id]
	if !ok || binding.Session != key || !r.isLiveLocked(key) || len(r.bySession[key]) != 1 {
		return false
	}
	binding.Ref = pane
	r.byPane[id] = binding
	r.bySession[key][id] = pane
	return true
}

// IsLiveRoute is the cheap final guard used after action-time local lookups.
// It detects liveness replacement or a same-pane rebind during validation.
func (r *Registry) IsLiveRoute(key ExactSessionKey, pane LocalPaneRef) bool {
	key = key.Canonical()
	r.mu.RLock()
	defer r.mu.RUnlock()
	bound, ok := r.byPane[identityOf(pane)]
	return ok && bound.Session == key && bound.Ref == pane && r.isLiveLocked(key) && len(r.bySession[key]) == 1
}

func (r *Registry) isLiveLocked(key ExactSessionKey) bool {
	_, ok := r.liveHost[key.Hostname][key]
	return ok
}
