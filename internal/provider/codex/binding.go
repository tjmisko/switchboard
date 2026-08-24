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

// BindingRegistry combines process-environment and hook identities. The
// process environment seeds identity before hooks arrive; a registered hook
// wins afterwards because it can observe /clear under the stable process while
// the process-start environment cannot change. CODEX_SESSION_ID is not used:
// 0.149 evidence shows that it can identify a parent rather than the current
// thread.
type BindingRegistry struct {
	env EnvironmentReader

	mu          sync.RWMutex
	hooks       map[provider.RootKey]string
	retired     map[provider.RootKey]map[string]struct{}
	environment map[provider.RootKey]string
}

func newBindingRegistry(env EnvironmentReader) *BindingRegistry {
	if env == nil {
		env = defaultEnvironmentReader()
	}
	return &BindingRegistry{
		env: env, hooks: make(map[provider.RootKey]string), retired: make(map[provider.RootKey]map[string]struct{}),
		environment: make(map[provider.RootKey]string),
	}
}

// RegisterHook records an exact hook-supplied thread ID for one process
// lifetime. It is safe to repeat with the same ID. A different trusted hook ID
// advances the binding for /clear under the same TUI process and retires the
// previous ID; a retired ID can never rotate the process backwards.
func (r *BindingRegistry) RegisterHook(key provider.RootKey, threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if key.PID <= 0 || key.StartedAt.IsZero() || threadID == "" {
		return errors.New("codex: hook binding requires pid, start identity, and thread id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, stale := r.retired[key][threadID]; stale {
		return fmt.Errorf("codex: retired hook binding for process lifetime")
	}
	if prior := r.hooks[key]; prior != "" && prior != threadID {
		if r.retired[key] == nil {
			r.retired[key] = make(map[string]struct{})
		}
		r.retired[key][prior] = struct{}{}
	}
	r.hooks[key] = threadID
	return nil
}

// Forget removes hook state for one process lifetime. It is idempotent.
func (r *BindingRegistry) Forget(key provider.RootKey) {
	r.mu.Lock()
	delete(r.hooks, key)
	delete(r.retired, key)
	delete(r.environment, key)
	r.mu.Unlock()
}

func (r *BindingRegistry) resolve(ctx context.Context, ref provider.RootRef) (Binding, string) {
	key := ref.Key()
	// A lifecycle hook can rotate the conversation under a stable TUI PID. Once
	// present it is newer than the process-start environment, whose value cannot
	// change after /clear.
	r.mu.RLock()
	if id := r.hooks[key]; id != "" {
		r.mu.RUnlock()
		return Binding{ThreadID: id, Source: BindingHook}, ""
	}
	r.mu.RUnlock()
	if r.env != nil {
		body, err := r.env.Environ(ctx, key)
		if err == nil {
			if id := environmentValue(body, "CODEX_THREAD_ID"); id != "" {
				r.mu.Lock()
				// A hook may have arrived while the environment read was in
				// flight. Preserve its newer, rotatable identity.
				if hookID := r.hooks[key]; hookID != "" {
					r.mu.Unlock()
					return Binding{ThreadID: hookID, Source: BindingHook}, ""
				}
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
