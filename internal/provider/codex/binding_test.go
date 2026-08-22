package codex

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/provider"
)

type fakeEnvironment struct {
	values map[provider.RootKey][]byte
	errors map[provider.RootKey]error
	calls  []provider.RootKey
}

func (f *fakeEnvironment) Environ(_ context.Context, key provider.RootKey) ([]byte, error) {
	f.calls = append(f.calls, key)
	return f.values[key], f.errors[key]
}

func TestBindingPrecedenceAndPIDStartIdentity(t *testing.T) {
	started := time.Unix(100, 0)
	key := provider.RootKey{PID: 42, StartedAt: started}
	environment := &fakeEnvironment{values: map[provider.RootKey][]byte{
		key: []byte("CODEX_SESSION_ID=session-parent\x00CODEX_THREAD_ID=environment-thread\x00"),
	}}
	registry := newBindingRegistry(environment)
	if _, err := registry.RegisterHook(key, "hook-thread"); err != nil {
		t.Fatal(err)
	}
	got, _ := registry.resolve(context.Background(), provider.RootRef{PID: key.PID, StartedAt: key.StartedAt, CWD: "/same"})
	if got.ThreadID != "hook-thread" || got.Source != BindingHook {
		t.Fatalf("environment precedence = %#v", got)
	}

	// PID reuse must not inherit the previous lifetime's environment or hook.
	reused := provider.RootRef{PID: key.PID, StartedAt: started.Add(time.Second), CWD: "/same"}
	got, diagnostic := registry.resolve(context.Background(), reused)
	if got.ThreadID != "" || diagnostic == "" {
		t.Fatalf("reused PID binding = %#v, diagnostic %q", got, diagnostic)
	}
	if len(environment.calls) != 1 || environment.calls[0] != reused.Key() {
		t.Fatalf("only the unbound reused lifetime should read its environment: %#v", environment.calls)
	}
}

func TestBindingIgnoresSessionIDAndCWDAndFallsBackToHook(t *testing.T) {
	keyA := provider.RootKey{PID: 1, StartedAt: time.Unix(1, 0)}
	keyB := provider.RootKey{PID: 2, StartedAt: time.Unix(2, 0)}
	environment := &fakeEnvironment{
		values: map[provider.RootKey][]byte{keyA: []byte("CODEX_SESSION_ID=not-the-thread\x00")},
		errors: map[provider.RootKey]error{keyB: errors.New("permission denied")},
	}
	registry := newBindingRegistry(environment)
	if _, err := registry.RegisterHook(keyA, "hook-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RegisterHook(keyB, "hook-b"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		key  provider.RootKey
		want string
	}{{keyA, "hook-a"}, {keyB, "hook-b"}} {
		got, _ := registry.resolve(context.Background(), provider.RootRef{PID: test.key.PID, StartedAt: test.key.StartedAt, CWD: "/identical"})
		if got.ThreadID != test.want || got.Source != BindingHook {
			t.Errorf("resolve(%#v) = %#v", test.key, got)
		}
	}
	registry.Forget(keyA)
	registry.Forget(keyA)
	if got, _ := registry.resolve(context.Background(), provider.RootRef{PID: keyA.PID, StartedAt: keyA.StartedAt, CWD: "/identical"}); got.ThreadID != "" {
		t.Fatalf("forgotten binding = %#v", got)
	}
}

func TestRegisterHookRotatesAndRejectsRetiredThread(t *testing.T) {
	registry := newBindingRegistry(&fakeEnvironment{})
	key := provider.RootKey{PID: 1, StartedAt: time.Unix(1, 0)}
	first, err := registry.RegisterHook(key, "one")
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation != 1 || first.Rotated {
		t.Fatalf("first binding = %+v", first)
	}
	second, err := registry.RegisterHook(key, "two")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Rotated || second.Generation != 2 {
		t.Fatalf("rotation = %+v", second)
	}
	stale, err := registry.RegisterHook(key, "one")
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Stale || stale.Generation != 2 {
		t.Fatalf("retired binding = %+v", stale)
	}
}
