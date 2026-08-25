package federation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestApplyLiveRoutesReplacesAndDropsHosts(t *testing.T) {
	registry := panebind.NewRegistry()
	known := make(map[string]struct{})
	startedAt := time.Unix(2, 0)
	key := panebind.ExactSessionKey{Hostname: "remote", PID: 2, StartedAt: startedAt}
	pane := panebind.LocalPaneRef{GUIPID: 10, WindowID: 11, PaneID: 12}
	if err := registry.Bind(key, pane); err != nil {
		t.Fatal(err)
	}

	applyLiveRoutes(map[string]state.Snapshot{
		"remote": {Sessions: []state.Session{{PID: 2, StartedAt: startedAt}}},
	}, known, registry, nil)
	if _, err := registry.Resolve(key); err != nil {
		t.Fatalf("live route not enabled: %v", err)
	}

	applyLiveRoutes(map[string]state.Snapshot{}, known, registry, nil)
	if _, err := registry.Resolve(key); !errors.Is(err, panebind.ErrSessionNotLive) {
		t.Fatalf("disconnected route error = %v", err)
	}

	// Reappearance of even the same exact identity requires a fresh binding.
	applyLiveRoutes(map[string]state.Snapshot{
		"remote": {Sessions: []state.Session{{PID: 2, StartedAt: startedAt}}},
	}, known, registry, nil)
	if _, err := registry.Resolve(key); !errors.Is(err, panebind.ErrSessionUnbound) {
		t.Fatalf("old candidate reauthorized after reconnect: %v", err)
	}
}

func TestRunLiveRoutesConsumesRemoteReplacements(t *testing.T) {
	remote := newFakeRemote()
	registry := panebind.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = RunLiveRoutes(ctx, remote, registry, nil)
		close(done)
	}()
	<-remote.ready

	startedAt := time.Unix(3, 0)
	key := panebind.ExactSessionKey{Hostname: "remote", PID: 3, StartedAt: startedAt}
	pane := panebind.LocalPaneRef{GUIPID: 20, WindowID: 21, PaneID: 22}
	if err := registry.Bind(key, pane); err != nil {
		t.Fatal(err)
	}
	remote.replace(map[string]state.Snapshot{"remote": {Sessions: []state.Session{{PID: 3, StartedAt: startedAt}}}})
	deadline := time.After(time.Second)
	for {
		if _, err := registry.Resolve(key); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("replacement did not reach route registry")
		default:
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("route synchronizer did not stop")
	}
}

type staleNotificationRemote struct {
	current map[string]state.Snapshot
	queued  chan map[string]state.Snapshot
}

func (r *staleNotificationRemote) Snapshot() map[string]state.Snapshot { return r.current }
func (r *staleNotificationRemote) Subscribe() (<-chan map[string]state.Snapshot, func()) {
	return r.queued, func() {}
}

func TestRunLiveRoutesTreatsQueuedValuesAsNotifications(t *testing.T) {
	startedAt := time.Unix(4, 0)
	key := panebind.ExactSessionKey{Hostname: "remote", PID: 4, StartedAt: startedAt}
	staleLive := map[string]state.Snapshot{"remote": {Sessions: []state.Session{{PID: 4, StartedAt: startedAt}}}}
	queued := make(chan map[string]state.Snapshot, 1)
	queued <- staleLive
	close(queued)
	remote := &staleNotificationRemote{current: map[string]state.Snapshot{}, queued: queued}
	registry := panebind.NewRegistry()
	if err := registry.Bind(key, panebind.LocalPaneRef{GUIPID: 30, WindowID: 31, PaneID: 32}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := RunLiveRoutes(ctx, remote, registry, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(key); !errors.Is(err, panebind.ErrSessionNotLive) {
		t.Fatalf("stale queued value resurrected route: %v", err)
	}
}
