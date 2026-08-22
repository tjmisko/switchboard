// Package provider defines the narrow orchestration seam between discovered
// root processes and provider-specific agent-graph observers.
package provider

import (
	"context"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// RootRef contains the discovery identity and exact provider binding inputs for
// one switchable root process. Observe may perform I/O and callers must invoke
// it outside the state-store lock.
type RootRef struct {
	PID               int
	StartedAt         time.Time
	SlotID            string
	ProviderEndpoint  string
	BindingGeneration uint64
	Provider          agentgraph.ProviderKind
	ProviderSessionID string
	Transcript        string
	CWD               string
}

// Key returns the PID-reuse-safe identity used by observers.
func (r RootRef) Key() RootKey {
	return RootKey{PID: r.PID, StartedAt: r.StartedAt, SlotID: r.SlotID}
}

// RootKey identifies one process lifetime. PID alone is insufficient because a
// later root can reuse it.
type RootKey struct {
	PID       int
	StartedAt time.Time
	SlotID    string
}

// Observer supplies immutable-boundary graph snapshots for roots. Observe may
// poll files or provider APIs, or read a long-running event-stream cache. Its
// returned Observation must be a deep copy that callers may mutate safely.
//
// Updates is a non-blocking, coalesced invalidation signal: receiving a key says
// to Observe that root again, not that the key is a complete state update.
// Losing an invalidation is safe because callers periodically reconcile.
// Forget is idempotent. Close stops internal goroutines and releases resources
// exactly once; implementations should make repeated calls harmless.
type Observer interface {
	Observe(context.Context, RootRef, time.Time) (agentgraph.Observation, error)
	Updates() <-chan RootKey
	Forget(RootKey)
	Close() error
}

// InvalidationQueue is a bounded non-blocking implementation helper for an
// Observer's Updates channel. A full queue drops later signals; periodic
// reconciliation is the required backstop. It does not own observer lifecycle
// and therefore is not closed independently, avoiding send/close races.
type InvalidationQueue struct {
	updates chan RootKey

	mu      sync.Mutex
	pending map[RootKey]struct{}
}

// NewInvalidationQueue constructs a queue with the given bounded capacity.
// Capacities below one are promoted to one.
func NewInvalidationQueue(capacity int) *InvalidationQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &InvalidationQueue{
		updates: make(chan RootKey, capacity),
		pending: make(map[RootKey]struct{}, capacity),
	}
}

// Signal attempts to enqueue an invalidation and never waits for a consumer.
// Repeated signals for a key already waiting in the channel are coalesced. The
// bookkeeping is reset after the channel drains; its only purpose is reducing
// a queued storm, not suppressing later observations of the same root.
func (q *InvalidationQueue) Signal(key RootKey) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.updates) == 0 {
		clear(q.pending)
	}
	if _, exists := q.pending[key]; exists {
		return
	}
	select {
	case q.updates <- key:
		q.pending[key] = struct{}{}
	default:
	}
}

// Updates returns the queue's receive-only invalidation channel.
func (q *InvalidationQueue) Updates() <-chan RootKey {
	return q.updates
}
