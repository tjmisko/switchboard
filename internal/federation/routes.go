package federation

import (
	"context"

	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/state"
)

// RouteDiagnostic receives only a canonical hostname and a finite validation
// error; it must not log remote payloads.
type RouteDiagnostic func(string, error)

// RunLiveRoutes projects complete remote snapshots into Registry's liveness
// gate. Candidate pane bindings are deliberately independent and may arrive
// first; this loop only decides when an exact candidate becomes actionable.
func RunLiveRoutes(ctx context.Context, source RemoteSource, registry *panebind.Registry, diagnostic RouteDiagnostic) error {
	return runLiveRoutes(ctx, source, registry, diagnostic, nil, nil)
}

// RunLiveRoutesReady adds a startup barrier and a post-replacement callback
// used to refresh aggregate route-ready metadata.
func RunLiveRoutesReady(ctx context.Context, source RemoteSource, registry *panebind.Registry, diagnostic RouteDiagnostic, changed func(), ready chan<- struct{}) error {
	return runLiveRoutes(ctx, source, registry, diagnostic, changed, ready)
}

func runLiveRoutes(ctx context.Context, source RemoteSource, registry *panebind.Registry, diagnostic RouteDiagnostic, changed func(), ready chan<- struct{}) error {
	if source == nil || registry == nil {
		if ready != nil {
			close(ready)
		}
		<-ctx.Done()
		return nil
	}
	updates, cancel := source.Subscribe()
	defer cancel()
	known := make(map[string]struct{})
	applyLiveRoutes(source.Snapshot(), known, registry, diagnostic)
	if changed != nil {
		changed()
	}
	if ready != nil {
		close(ready)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-updates:
			if !ok {
				for host := range known {
					registry.DropLiveHost(host)
				}
				if changed != nil {
					changed()
				}
				return nil
			}
			// Channel values are notifications, not revisions. A notification
			// queued before the Snapshot above may be older than it; always read
			// the source's current full replacement to avoid rolling liveness
			// backward or briefly resurrecting a disconnected route.
			applyLiveRoutes(source.Snapshot(), known, registry, diagnostic)
			if changed != nil {
				changed()
			}
		}
	}
}

func applyLiveRoutes(snapshots map[string]state.Snapshot, known map[string]struct{}, registry *panebind.Registry, diagnostic RouteDiagnostic) {
	for host := range known {
		if _, live := snapshots[host]; !live {
			registry.DropLiveHost(host)
			delete(known, host)
		}
	}
	for host, snapshot := range snapshots {
		keys := make([]panebind.ExactSessionKey, 0, len(snapshot.Sessions))
		for _, session := range snapshot.Sessions {
			keys = append(keys, panebind.ExactSessionKey{
				Hostname: host, PID: session.PID, StartedAt: session.StartedAt,
			})
		}
		if err := registry.ReplaceLive(host, keys); err != nil {
			registry.DropLiveHost(host)
			delete(known, host)
			if diagnostic != nil {
				diagnostic(host, err)
			}
			continue
		}
		known[host] = struct{}{}
	}
}
