// Command switchboard is the daemon. It runs one long-lived process per
// user session, watches /proc for claude binaries, owns pidfds for instant
// death detection, listens to Hyprland's socket2 for window lifecycle, and
// serves an RPC socket for waybar + ctl.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/detect"
	"github.com/tjmisko/switchboard/internal/discovery"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/wm"
)

func main() {
	statePath := flag.String("state", defaultStatePath(), "path to state.json mirror")
	socketPath := flag.String("socket", defaultSocketPath(), "path to RPC unix socket")
	scanInterval := flag.Duration("scan-interval", 1*time.Second, "/proc scan interval")
	reconcileInterval := flag.Duration("reconcile-interval", 5*time.Second, "full reconcile interval")
	wmFlag := flag.String("wm", "auto", "WM backend: auto|hyprland|sway|i3|x11|none")
	terminalFlag := flag.String("terminal", "auto", "terminal backend: auto|wezterm|tmux|none")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	stack := detect.Detect(detect.Options{WM: *wmFlag, Terminal: *terminalFlag})
	caps := stack.Capabilities()
	log.Printf("backends: wm=%s terminal=%s observe=%t navigate=%t",
		caps.WM, caps.Terminal, caps.Observe, caps.Navigate)

	store := state.New(*statePath)
	store.SetCapabilities(caps)
	if err := store.Load(); err != nil {
		log.Printf("hydrate: %v (continuing)", err)
	}
	dropStaleSessions(store)

	procSrc := stack.OSProc
	term := stack.Terminal
	manager := stack.WM
	scanner := discovery.New()
	resolver := mapping.NewResolver(term, manager)

	onClaudeAppeared := func(info proc.Info) {
		log.Printf("claude pid=%d cwd=%s tty=%s discovered", info.PID, info.CWD, info.TTY)
		sess := resolver.Resolve(ctx, info)
		store.Apply(func(m map[int]*state.Session) { m[sess.PID] = &sess })

		if err := procSrc.Watch(ctx, info.PID, func() {
			log.Printf("claude pid=%d died", info.PID)
			store.Apply(func(m map[int]*state.Session) { delete(m, info.PID) })
			scanner.Forget(info.PID)
		}); err != nil {
			log.Printf("watch pid=%d: %v", info.PID, err)
		}
	}

	go func() {
		if err := scanner.Run(ctx, *scanInterval, onClaudeAppeared); err != nil && ctx.Err() == nil {
			log.Printf("scanner: %v", err)
		}
	}()
	go runWMLoop(ctx, store, resolver, manager)
	go runReconciler(ctx, store, resolver, manager, stack, *reconcileInterval)

	server := rpc.New(store, *socketPath, term, manager)
	if err := os.MkdirAll(filepath.Dir(*socketPath), 0o755); err != nil {
		log.Fatalf("mkdir socket dir: %v", err)
	}
	log.Printf("switchboard listening on %s", *socketPath)
	if err := server.Serve(ctx); err != nil {
		log.Fatalf("rpc: %v", err)
	}
}

// dropStaleSessions removes hydrated sessions whose PID is gone or no longer
// looks like claude. Run once at startup, before any live discovery — the
// scanner will re-add survivors on the first tick.
func dropStaleSessions(store *state.Store) {
	store.Apply(func(m map[int]*state.Session) {
		for pid := range m {
			info, err := proc.Read(pid)
			if err != nil || !discovery.IsClaude(info) {
				delete(m, pid)
			}
		}
	})
}

func runWMLoop(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager) {
	for ctx.Err() == nil {
		events, err := manager.Subscribe(ctx)
		if err != nil {
			log.Printf("wm subscribe: %v (retrying in 2s)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for evt := range events {
			handleWMEvent(ctx, store, resolver, evt)
		}
		// channel closed (connection EOF or ctx cancel) — loop will retry
	}
}

// handleWMEvent reacts to a neutral window event. Addresses arrive already
// normalized to Clients() form (the wm seam owns the Hyprland 0x quirk), so the
// daemon compares them directly against sess.Hyprland.Address.
func handleWMEvent(ctx context.Context, store *state.Store, resolver *mapping.Resolver, evt wm.Event) {
	switch evt.Kind {
	case wm.EventWindowClosed:
		// Drop any session living in the closed window. Covers the "user closed
		// the terminal while claude was running" case.
		store.Apply(func(m map[int]*state.Session) {
			for pid, sess := range m {
				if sess.Hyprland != nil && sess.Hyprland.Address == evt.Address {
					delete(m, pid)
				}
			}
		})
	case wm.EventFocusChanged:
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				if sess.Hyprland == nil {
					sess.Focused = false
					continue
				}
				sess.Focused = sess.Hyprland.Address == evt.Address
			}
		})
	case wm.EventLayoutChanged:
		// Something changed — kick a reconcile on any session that might match.
		// Cheap: just iterate live sessions and re-resolve.
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				resolver.Reconcile(ctx, sess)
			}
		})
	}
}

// runReconciler periodically re-resolves every session's wezterm + hyprland
// mapping and re-syncs the Focused flag against the current active window.
// Catches anything missed by event-driven updates (e.g. a session whose
// mapping was incomplete when first created, the initial focus state, or a
// hyprctl race).
func runReconciler(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	reconcileOnce(ctx, store, resolver, manager, stack)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcileOnce(ctx, store, resolver, manager, stack)
		}
	}
}

func reconcileOnce(ctx context.Context, store *state.Store, resolver *mapping.Resolver, manager wm.Manager, stack detect.Stack) {
	// Re-publish capabilities every tick: the terminal locator is self-redetecting
	// (detect.NewAuto), so a terminal that came up after the daemon flips
	// terminal/navigate from their boot-race "none" values without a restart.
	store.SetCapabilities(stack.Capabilities())
	active, _ := manager.ActiveWindow(ctx)
	store.Apply(func(m map[int]*state.Session) {
		for _, sess := range m {
			resolver.Reconcile(ctx, sess)
			if sess.Hyprland != nil {
				sess.Focused = sess.Hyprland.Address == active
			}
			// Refresh job-control suspension (Ctrl-Z). On ErrGone the procwatch
			// death callback will drop the session shortly; leave the last-known
			// value until then rather than flapping.
			if st, err := proc.State(sess.PID); err == nil {
				sess.Suspended = proc.Suspended(st)
			}
		}
	})
}

func defaultStatePath() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "switchboard", "state.json")
	}
	return filepath.Join(os.Getenv("HOME"), ".cache", "switchboard", "state.json")
}

func defaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "switchboard.sock")
	}
	return fmt.Sprintf("/tmp/switchboard-%d.sock", os.Getuid())
}
