package codex

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/provider"
)

func TestBindingRegistryRequiresExactHookIdentity(t *testing.T) {
	registry := newBindingRegistry()
	key := provider.RootKey{PID: 12, StartedAt: time.Unix(10, 0)}
	ref := provider.RootRef{PID: key.PID, StartedAt: key.StartedAt}
	if got, diagnostic := registry.resolve(ref); got.ThreadID != "" || diagnostic != "exact hook binding unavailable" {
		t.Fatalf("resolve before hook = %+v, %q", got, diagnostic)
	}
	update, err := registry.RegisterHook(key, "thread-one")
	if err != nil || update.ThreadID != "thread-one" || update.Rotated || update.Stale {
		t.Fatalf("first hook binding = %+v, %v", update, err)
	}
	if got, diagnostic := registry.resolve(ref); got.ThreadID != "thread-one" || got.Source != BindingHook || diagnostic != "" {
		t.Fatalf("resolved hook = %+v, %q", got, diagnostic)
	}
}

func TestBindingRegistryRotatesForwardAndFencesRetiredThread(t *testing.T) {
	registry := newBindingRegistry()
	key := provider.RootKey{PID: 12, StartedAt: time.Unix(10, 0)}
	if _, err := registry.RegisterHook(key, "thread-one"); err != nil {
		t.Fatal(err)
	}
	rotated, err := registry.RegisterHook(key, "thread-two")
	if err != nil || !rotated.Rotated {
		t.Fatalf("rotation = %+v, %v", rotated, err)
	}
	stale, err := registry.RegisterHook(key, "thread-one")
	if err != nil || !stale.Stale {
		t.Fatalf("retired hook = %+v, %v", stale, err)
	}
	got, _ := registry.resolve(provider.RootRef{PID: key.PID, StartedAt: key.StartedAt})
	if got.ThreadID != "thread-two" {
		t.Fatalf("stale hook changed binding: %+v", got)
	}
}

func TestBindingRegistryScopesPIDReuseByStartTime(t *testing.T) {
	registry := newBindingRegistry()
	oldKey := provider.RootKey{PID: 12, StartedAt: time.Unix(10, 0)}
	newKey := provider.RootKey{PID: 12, StartedAt: time.Unix(20, 0)}
	_, _ = registry.RegisterHook(oldKey, "old-thread")
	_, _ = registry.RegisterHook(newKey, "new-thread")
	old, _ := registry.resolve(provider.RootRef{PID: 12, StartedAt: oldKey.StartedAt})
	newer, _ := registry.resolve(provider.RootRef{PID: 12, StartedAt: newKey.StartedAt})
	if old.ThreadID != "old-thread" || newer.ThreadID != "new-thread" {
		t.Fatalf("process lifetimes crossed: old=%+v new=%+v", old, newer)
	}
}
