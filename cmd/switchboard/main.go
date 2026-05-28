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
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/discovery"
	"github.com/tjmisko/switchboard/internal/hyprland"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
)

func main() {
	statePath := flag.String("state", defaultStatePath(), "path to state.json mirror")
	socketPath := flag.String("socket", defaultSocketPath(), "path to RPC unix socket")
	scanInterval := flag.Duration("scan-interval", 1*time.Second, "/proc scan interval")
	reconcileInterval := flag.Duration("reconcile-interval", 5*time.Second, "full reconcile interval")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	store := state.New(*statePath)
	if err := store.Load(); err != nil {
		log.Printf("hydrate: %v (continuing)", err)
	}
	dropStaleSessions(store)

	procSrc := osproc.New()
	scanner := discovery.New()
	term := terminal.NewWezterm()
	resolver := mapping.NewResolver(term)

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
	go runHyprlandLoop(ctx, store, resolver)
	go runReconciler(ctx, store, resolver, *reconcileInterval)

	server := rpc.New(store, *socketPath, term)
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

func runHyprlandLoop(ctx context.Context, store *state.Store, resolver *mapping.Resolver) {
	for ctx.Err() == nil {
		events, err := hyprland.Subscribe(ctx)
		if err != nil {
			log.Printf("hyprland subscribe: %v (retrying in 2s)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for evt := range events {
			handleHyprlandEvent(ctx, store, resolver, evt)
		}
		// channel closed (socket EOF or ctx cancel) — loop will retry
	}
}

func handleHyprlandEvent(ctx context.Context, store *state.Store, resolver *mapping.Resolver, evt hyprland.Event) {
	switch evt.Name {
	case "closewindow":
		// closed window address — drop any session living in it. Covers the
		// "user closed the terminal while claude was running" case.
		addr := "0x" + evt.Data
		store.Apply(func(m map[int]*state.Session) {
			for pid, sess := range m {
				if sess.Hyprland != nil && sess.Hyprland.Address == addr {
					delete(m, pid)
				}
			}
		})
	case "activewindowv2":
		addr := "0x" + evt.Data
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				if sess.Hyprland == nil {
					sess.Focused = false
					continue
				}
				sess.Focused = sess.Hyprland.Address == addr
			}
		})
	case "movewindowv2", "windowtitlev2", "openwindow":
		// Something changed — kick a reconcile on any session that might
		// match. Cheap: just iterate live sessions and re-resolve.
		store.Apply(func(m map[int]*state.Session) {
			for _, sess := range m {
				resolver.Reconcile(ctx, sess)
			}
		})
	}
	_ = strings.TrimSpace // keep import alive if we strip handlers later
}

// runReconciler periodically re-resolves every session's wezterm + hyprland
// mapping and re-syncs the Focused flag against the current active window.
// Catches anything missed by event-driven updates (e.g. a session whose
// mapping was incomplete when first created, the initial focus state, or a
// hyprctl race).
func runReconciler(ctx context.Context, store *state.Store, resolver *mapping.Resolver, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	reconcileOnce(ctx, store, resolver)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcileOnce(ctx, store, resolver)
		}
	}
}

func reconcileOnce(ctx context.Context, store *state.Store, resolver *mapping.Resolver) {
	active, _ := hyprland.ActiveWindowAddress(ctx)
	store.Apply(func(m map[int]*state.Session) {
		for _, sess := range m {
			resolver.Reconcile(ctx, sess)
			if sess.Hyprland != nil {
				sess.Focused = sess.Hyprland.Address == active
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
