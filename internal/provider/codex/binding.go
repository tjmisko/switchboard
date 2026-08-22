package codex

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/tjmisko/switchboard/internal/provider"
)

// EnvironmentReader is the process-environment seam used for exact Codex
// thread binding. Implementations must treat RootKey as the process lifetime,
// not PID alone. Tests and non-Linux callers can inject an implementation.
type EnvironmentReader interface {
	Environ(context.Context, provider.RootKey) ([]byte, error)
}

// BindingSource records which exact identity source won. CWD, timestamps, and
// rollout recency are deliberately not binding sources.
type BindingSource string

const (
	BindingUnbound            BindingSource = ""
	BindingProcessEnvironment BindingSource = "process_environment"
	BindingHook               BindingSource = "hook"
)

// Binding is an exact root-thread association. An empty ThreadID is unbound.
type Binding struct {
	ThreadID   string
	Source     BindingSource
	Generation uint64
}

type BindingUpdate struct {
	ThreadID   string
	Generation uint64
	Rotated    bool
	Stale      bool
}

type bindingRecord struct {
	threadID   string
	generation uint64
	retired    map[string]struct{}
}

// BindingRegistry combines process-environment and hook identities. The
// environment supplies a startup fallback; an exact hook wins after arrival
// because /clear changes the thread without replacing the process.
type BindingRegistry struct {
	env EnvironmentReader

	mu          sync.RWMutex
	hooks       map[provider.RootKey]*bindingRecord
	environment map[provider.RootKey]string
}

func newBindingRegistry(env EnvironmentReader) *BindingRegistry {
	if env == nil {
		env = defaultEnvironmentReader()
	}
	return &BindingRegistry{
		env: env, hooks: make(map[provider.RootKey]*bindingRecord),
		environment: make(map[provider.RootKey]string),
	}
}

// RegisterHook records an exact hook-supplied thread ID for one process
// lifetime. A different non-retired ID is a /clear rotation, never a conflict.
func (r *BindingRegistry) RegisterHook(key provider.RootKey, threadID string) (BindingUpdate, error) {
	threadID = strings.TrimSpace(threadID)
	if key.PID <= 0 || key.StartedAt.IsZero() || threadID == "" {
		return BindingUpdate{}, errors.New("codex: hook binding requires pid, start identity, and thread id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.hooks[key]
	if record == nil {
		record = &bindingRecord{generation: 1, retired: make(map[string]struct{})}
		r.hooks[key] = record
	}
	if record.threadID == threadID {
		return BindingUpdate{ThreadID: threadID, Generation: record.generation}, nil
	}
	if _, stale := record.retired[threadID]; stale {
		return BindingUpdate{ThreadID: threadID, Generation: record.generation, Stale: true}, nil
	}
	rotated := record.threadID != ""
	if rotated {
		record.retired[record.threadID] = struct{}{}
		record.generation++
	}
	record.threadID = threadID
	return BindingUpdate{ThreadID: threadID, Generation: record.generation, Rotated: rotated}, nil
}

// Forget removes hook state for one process lifetime. It is idempotent.
func (r *BindingRegistry) Forget(key provider.RootKey) {
	r.mu.Lock()
	delete(r.hooks, key)
	delete(r.environment, key)
	r.mu.Unlock()
}

func (r *BindingRegistry) resolve(ctx context.Context, ref provider.RootRef) (Binding, string) {
	key := ref.Key()
	r.mu.RLock()
	if hook := r.hooks[key]; hook != nil && hook.threadID != "" {
		binding := Binding{ThreadID: hook.threadID, Source: BindingHook, Generation: hook.generation}
		r.mu.RUnlock()
		return binding, ""
	}
	r.mu.RUnlock()
	if r.env != nil {
		body, err := r.env.Environ(ctx, key)
		if err == nil {
			if id := environmentValue(body, "CODEX_THREAD_ID"); id != "" {
				r.mu.Lock()
				r.environment[key] = id
				r.mu.Unlock()
				return Binding{ThreadID: id, Source: BindingProcessEnvironment, Generation: 1}, ""
			}
		} else if ctx.Err() != nil {
			return Binding{}, "binding cancelled"
		}
		// Permission, process-exit, and malformed environment reads are ordinary
		// unbound cases; never include an OS error or environment contents in the
		// in-memory diagnostic.
	}
	r.mu.RLock()
	if id := r.environment[key]; id != "" {
		r.mu.RUnlock()
		return Binding{ThreadID: id, Source: BindingProcessEnvironment, Generation: 1}, ""
	}
	r.mu.RUnlock()
	return Binding{}, "exact Codex thread binding unavailable"
}

func environmentValue(body []byte, key string) string {
	prefix := key + "="
	for record := range strings.SplitSeq(string(body), "\x00") {
		if strings.HasPrefix(record, prefix) {
			value := strings.TrimSpace(strings.TrimPrefix(record, prefix))
			if value != "" && !strings.ContainsRune(value, '\x00') {
				return value
			}
		}
	}
	return ""
}
