package codex

import (
	"context"
	"errors"
	"fmt"
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
	ThreadID string
	Source   BindingSource
}

// BindingRegistry combines process-environment and hook identities. Process
// environment wins when CODEX_THREAD_ID is present. CODEX_SESSION_ID is not
// used: 0.149 evidence shows that it can identify a parent rather than the
// current thread.
type BindingRegistry struct {
	env EnvironmentReader

	mu          sync.RWMutex
	hooks       map[provider.RootKey]string
	environment map[provider.RootKey]string
}

func newBindingRegistry(env EnvironmentReader) *BindingRegistry {
	if env == nil {
		env = defaultEnvironmentReader()
	}
	return &BindingRegistry{
		env: env, hooks: make(map[provider.RootKey]string),
		environment: make(map[provider.RootKey]string),
	}
}

// RegisterHook records an exact hook-supplied thread ID for one process
// lifetime. It is safe to repeat with the same ID; conflicting exact IDs are
// rejected instead of being silently replaced.
func (r *BindingRegistry) RegisterHook(key provider.RootKey, threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if key.PID <= 0 || key.StartedAt.IsZero() || threadID == "" {
		return errors.New("codex: hook binding requires pid, start identity, and thread id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prior := r.hooks[key]; prior != "" && prior != threadID {
		return fmt.Errorf("codex: conflicting hook binding for process lifetime")
	}
	r.hooks[key] = threadID
	return nil
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
	if r.env != nil {
		body, err := r.env.Environ(ctx, key)
		if err == nil {
			if id := environmentValue(body, "CODEX_THREAD_ID"); id != "" {
				r.mu.Lock()
				r.environment[key] = id
				r.mu.Unlock()
				return Binding{ThreadID: id, Source: BindingProcessEnvironment}, ""
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
		return Binding{ThreadID: id, Source: BindingProcessEnvironment}, ""
	}
	id := r.hooks[key]
	r.mu.RUnlock()
	if id != "" {
		return Binding{ThreadID: id, Source: BindingHook}, ""
	}
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
