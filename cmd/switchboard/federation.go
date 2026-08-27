package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/federation"
	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/remotestate"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/wm"
)

// remoteDestinations is a repeatable -remote flag. Destinations remain local
// configuration; no remote frame or OSC value can choose an SSH target.
type remoteDestinations []string

func (r *remoteDestinations) String() string { return strings.Join(*r, ",") }
func (r *remoteDestinations) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("remote destination is empty")
	}
	*r = append(*r, value)
	return nil
}

type federationRuntime struct {
	hostname   string
	store      *state.Store
	manager    *remotestate.Manager
	registry   *panebind.Registry
	view       *federation.View
	navigator  *federation.Navigator
	workspaces *federation.WorkspaceIndex
	announcer  panebind.Announcer
	wg         sync.WaitGroup
}

// remoteHoldConfig is the client's hysteresis policy for remote rows. It is
// separated from the destination list because it is a display/trust decision,
// not a topology one.
type remoteHoldConfig struct {
	// HoldFor is how long a host's last observation stands after contact is lost
	// with no closeout. Zero removes rows at the disconnect.
	HoldFor time.Duration
	// QuietFor is how much of HoldFor passes with no observable change before
	// held rows are marked stale.
	QuietFor time.Duration
}

func newFederationRuntime(store *state.Store, manager wm.Manager, destinations []string, hold remoteHoldConfig) (*federationRuntime, error) {
	rawHostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("hostname: %w", err)
	}
	hostname, err := remotestate.CanonicalHostname(rawHostname)
	if err != nil {
		return nil, fmt.Errorf("canonical hostname: %w", err)
	}
	registry := panebind.NewRegistry()
	runtime := &federationRuntime{
		hostname:   hostname,
		store:      store,
		registry:   registry,
		workspaces: federation.NewWorkspaceIndex(registry),
		announcer:  panebind.NewAnnouncer(),
	}
	remoteManager, err := remotestate.NewManager(remotestate.ManagerConfig{
		Destinations:  destinations,
		LocalHostname: hostname,
		HoldFor:       hold.HoldFor,
		QuietFor:      hold.QuietFor,
		OnDiagnostic: func(diagnostic remotestate.Diagnostic) {
			// Destination is trusted local configuration and category is finite;
			// the reason is a peer-supplied token that DecodeFrame has already
			// constrained to [a-z0-9_-]{0,32}, so it cannot forge a log record.
			// Never include SSH stderr or a peer-controlled frame.
			reason := ""
			if diagnostic.Reason != "" {
				reason = " reason=" + diagnostic.Reason
			}
			log.Printf("remote-state: destination=%s host=%s category=%s%s",
				diagnostic.Destination, diagnostic.Host, diagnostic.Category, reason)
		},
		OnHostRemoved: func(host string) {
			runtime.registry.DropLiveHost(host)
			if runtime.view != nil {
				runtime.view.DropRemoteHost(host)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	runtime.manager = remoteManager
	runtime.view, err = federation.NewView(store, hostname, remoteManager)
	if err != nil {
		return nil, err
	}
	localResolver := panebind.NewLocalResolver(manager)
	runtime.navigator = &federation.Navigator{
		LocalHostname: hostname,
		View:          runtime.view,
		Registry:      runtime.registry,
		Routes:        panebind.RouteResolver{Registry: runtime.registry, Local: localResolver},
		WM:            manager,
	}
	runtime.view.SetRouteReady(runtime.navigator.RouteReady)
	runtime.view.SetRouteWorkspace(runtime.workspaces.Workspace)
	return runtime, nil
}

// ObserveWindows feeds the reconcile tick's WM client enumeration to the
// workspace index, which is what lets a remote chip sit at the local workspace
// showing it. Install it on the mapping resolver, whose Enumerate already
// fetches that list once per tick.
func (f *federationRuntime) ObserveWindows(clients []wm.Window) {
	f.workspaces.ObserveWindows(clients)
}

func (f *federationRuntime) ConfigureServer(server *rpc.Server) {
	f.navigator.FocusLocal = server.FocusLocalSession
	server.SetFederation(f.view, f.navigator.Focus)
	server.SetPaneBinding(f.navigator.BindPane, f.navigator.PaneState, f.AnnounceAll)
}

// StartViews installs both subscriptions before the RPC socket is made ready.
// That lets an immediate OSC callback safely arrive before the first remote
// snapshot: it remains a candidate until RunLiveRoutes sees the full frame.
func (f *federationRuntime) StartViews(ctx context.Context) error {
	viewReady := make(chan struct{})
	routesReady := make(chan struct{})
	f.wg.Add(2)
	go func() {
		defer f.wg.Done()
		if err := f.view.RunReady(ctx, viewReady); err != nil && ctx.Err() == nil {
			log.Printf("federation view: %v", err)
		}
	}()
	go func() {
		defer f.wg.Done()
		if err := federation.RunLiveRoutesReady(ctx, f.manager, f.registry, func(host string, err error) {
			log.Printf("remote routes: host=%s category=invalid_live_set", host)
		}, f.view.Refresh, routesReady); err != nil && ctx.Err() == nil {
			log.Printf("remote routes: %v", err)
		}
	}()
	for viewReady != nil || routesReady != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-viewReady:
			viewReady = nil
		case <-routesReady:
			routesReady = nil
		}
	}
	return nil
}

// StartRemotes must run only after rpc.Server.ServeReady has closed its ready
// channel; remote-stream's attach announcement dials that socket immediately.
func (f *federationRuntime) StartRemotes(ctx context.Context) {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		if err := f.manager.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("remote-state manager: %v", err)
		}
	}()
}

func (f *federationRuntime) Wait() { f.wg.Wait() }

func (f *federationRuntime) liveTarget(key panebind.ExactSessionKey, tty string) bool {
	if key.Hostname != f.hostname {
		return false
	}
	for _, session := range f.store.Snapshot().Sessions {
		if session.PID == key.PID && session.StartedAt.Equal(key.StartedAt) &&
			session.TTY == tty && !session.Headless {
			return true
		}
	}
	return false
}

// AnnounceSession is best-effort Observe enrichment. It performs no Store
// mutation and Announcer revalidates the exact live tuple after opening the TTY.
func (f *federationRuntime) AnnounceSession(ctx context.Context, session state.Session) {
	if session.Headless || session.TTY == "" || session.PID <= 0 || session.StartedAt.IsZero() {
		return
	}
	target := panebind.Target{
		Session: panebind.ExactSessionKey{Hostname: f.hostname, PID: session.PID, StartedAt: session.StartedAt},
		TTY:     session.TTY,
	}
	if err := f.announcer.Announce(ctx, target, f.liveTarget); err != nil && ctx.Err() == nil {
		// Keep terminal paths and raw payloads out of the log.
		log.Printf("pane binding: pid=%d category=announce_failed", session.PID)
	}
}

// AnnounceAll is called synchronously before a remote-stream subscription. A
// failed TTY remains observe-only and does not fail the state stream.
func (f *federationRuntime) AnnounceAll(ctx context.Context) error {
	for _, session := range f.store.Snapshot().Sessions {
		f.AnnounceSession(ctx, session)
	}
	return nil
}
