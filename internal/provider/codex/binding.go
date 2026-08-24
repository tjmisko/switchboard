package codex

import (
	"errors"
	"strings"
	"sync"

	"github.com/tjmisko/switchboard/internal/provider"
)

// BindingSource records which exact identity source won. CWD, timestamps, and
// rollout recency are deliberately not binding sources.
type BindingSource string

const (
	BindingUnbound BindingSource = ""
	BindingHook    BindingSource = "hook"
)

// Binding is an exact root-thread association. An empty ThreadID is unbound.
type Binding struct {
	ThreadID string
	Source   BindingSource
}

type BindingUpdate struct {
	ThreadID string
	Rotated  bool
	Stale    bool
}

type bindingRecord struct {
	threadID string
	retired  map[string]struct{}
}

// BindingRegistry retains only exact hook-supplied identities. Process
// environment, cwd, recency, and loaded-thread discovery are intentionally not
// binding sources.
type BindingRegistry struct {
	mu    sync.RWMutex
	hooks map[provider.RootKey]*bindingRecord
}

func newBindingRegistry() *BindingRegistry {
	return &BindingRegistry{hooks: make(map[provider.RootKey]*bindingRecord)}
}

// RegisterHook records an exact hook-supplied thread ID for one process
// lifetime. It is safe to repeat with the same ID. A different trusted hook ID
// advances the binding for /clear under the same TUI process and retires the
// previous ID; a retired ID can never rotate the process backwards.
func (r *BindingRegistry) RegisterHook(key provider.RootKey, threadID string) (BindingUpdate, error) {
	threadID = strings.TrimSpace(threadID)
	if key.PID <= 0 || key.StartedAt.IsZero() || threadID == "" {
		return BindingUpdate{}, errors.New("codex: hook binding requires pid, start identity, and thread id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.hooks[key]
	if record == nil {
		record = &bindingRecord{retired: make(map[string]struct{})}
		r.hooks[key] = record
	}
	if record.threadID == threadID {
		return BindingUpdate{ThreadID: threadID}, nil
	}
	if _, stale := record.retired[threadID]; stale {
		return BindingUpdate{ThreadID: threadID, Stale: true}, nil
	}
	rotated := record.threadID != ""
	if rotated {
		record.retired[record.threadID] = struct{}{}
	}
	record.threadID = threadID
	return BindingUpdate{ThreadID: threadID, Rotated: rotated}, nil
}

// Forget removes hook state for one process lifetime. It is idempotent.
func (r *BindingRegistry) Forget(key provider.RootKey) {
	r.mu.Lock()
	delete(r.hooks, key)
	r.mu.Unlock()
}

func (r *BindingRegistry) resolve(ref provider.RootRef) (Binding, string) {
	key := ref.Key()
	r.mu.RLock()
	if hook := r.hooks[key]; hook != nil && hook.threadID != "" {
		binding := Binding{ThreadID: hook.threadID, Source: BindingHook}
		r.mu.RUnlock()
		return binding, ""
	}
	r.mu.RUnlock()
	return Binding{}, "exact hook binding unavailable"
}
