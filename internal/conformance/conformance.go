// Package conformance holds the backend-agnostic contract suites for the three
// portability seams. Each Run*Contract is parameterized over an implementation
// of a minimal local interface and asserts only *neutral observable* behavior —
// never OS mechanism. The same suite therefore validates every backend:
// Linux/Hyprland/wezterm today, sway/i3/X11/tmux/macOS in later phases. Phase 1's
// internal/osproc, internal/wm, and internal/terminal packages adopt these
// contracts directly (their concrete types satisfy these interfaces and call
// Run*Contract from their tests).
//
// Suites split into an always-runnable core (sentinels, tty-non-empty, event
// translation, address normalization) and live portions that need a running
// backend; the latter SKIP cleanly when the backend is unavailable, so CI stays
// green on a host with no compositor or terminal multiplexer.
package conformance

import (
	"context"
	"os"
	"testing"
	"time"
)

// liveConformance reports whether the live-backend portions of the WM/terminal
// contracts should run. They are opt-in (SWITCHBOARD_LIVE_CONFORMANCE=1) so the
// default `go test ./...` is fast and deterministic — it never depends on a
// running compositor/terminal and never pokes the user's live session. CI runs
// the always-on core; a developer or a backend CI lane sets the flag to drive
// the live assertions on a host that actually has the backend.
func liveConformance() bool { return os.Getenv("SWITCHBOARD_LIVE_CONFORMANCE") != "" }

// ---------------------------------------------------------------------------
// Seam 1 — OS process source
// ---------------------------------------------------------------------------

// ProcInfo is the neutral process record. Fields mirror what every OS backend
// can supply; TTY is an opaque join key (its literal form is OS-specific).
type ProcInfo struct {
	PID  int
	PPID int
	Comm string
	Exe  string
	CWD  string
	TTY  string
}

// Source enumerates processes and signals once when a watched pid dies.
type Source interface {
	Enumerate() ([]ProcInfo, error)
	Read(pid int) (ProcInfo, error)
	Watch(ctx context.Context, pid int, onDeath func()) error
	Stop(pid int)
}

// SourceFixture bundles the implementation with the host-specific helpers the
// neutral suite cannot synthesize itself.
type SourceFixture struct {
	Source Source

	// IsGone reports whether err is the implementation's "process disappeared"
	// sentinel (Linux: proc.ErrGone; macOS: its equivalent).
	IsGone func(error) bool

	// SpawnTTYChild starts a live child with a controlling terminal and returns
	// its pid; cleanup is the fixture's responsibility (t.Cleanup).
	SpawnTTYChild func(t *testing.T) int
	// SpawnBareChild starts a live child WITHOUT a tty and returns its pid.
	SpawnBareChild func(t *testing.T) int
	// KillChild kills and reaps the given child pid.
	KillChild func(t *testing.T, pid int)

	// MaskedExePID returns a pid whose exe/cwd are unobtainable (e.g. a kernel
	// thread) and true, or 0/false if the host has none (the case is skipped).
	MaskedExePID func() (int, bool)
}

// RunSourceContract asserts the neutral OS-process-source contract.
func RunSourceContract(t *testing.T, fx SourceFixture) {
	t.Helper()

	t.Run("enumerate reports a non-empty tty and cwd for an interactive child", func(t *testing.T) {
		pid := fx.SpawnTTYChild(t)
		var found *ProcInfo
		infos, err := fx.Source.Enumerate()
		if err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
		for i := range infos {
			if infos[i].PID == pid {
				found = &infos[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("Enumerate did not include the interactive child pid %d", pid)
		}
		// Assert the NEUTRAL observable, never the /dev/pts/ prefix.
		if found.TTY == "" {
			t.Errorf("interactive child tty is empty, want non-empty")
		}
		if found.CWD == "" {
			t.Errorf("child cwd is empty, want non-empty")
		}
	})

	t.Run("a non-interactive child has an empty tty", func(t *testing.T) {
		pid := fx.SpawnBareChild(t)
		info, err := fx.Source.Read(pid)
		if err != nil {
			t.Fatalf("Read(bare child): %v", err)
		}
		if info.TTY != "" {
			t.Errorf("bare child tty = %q, want empty", info.TTY)
		}
	})

	t.Run("read of a dead pid is the gone sentinel", func(t *testing.T) {
		pid := fx.SpawnBareChild(t)
		fx.KillChild(t, pid)
		if _, err := fx.Source.Read(pid); !fx.IsGone(err) {
			t.Errorf("Read(dead) err = %v, want gone sentinel", err)
		}
	})

	t.Run("exe/cwd are empty (not an error) when unobtainable", func(t *testing.T) {
		pid, ok := fx.MaskedExePID()
		if !ok {
			t.Skip("no masked-exe pid available on this host")
		}
		info, err := fx.Source.Read(pid)
		if err != nil {
			t.Fatalf("Read(masked) returned error %v, want nil with empty exe", err)
		}
		if info.Exe != "" {
			t.Errorf("masked exe = %q, want empty", info.Exe)
		}
	})

	t.Run("watch fires onDeath exactly once on death", func(t *testing.T) {
		pid := fx.SpawnBareChild(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fired := make(chan struct{}, 4)
		if err := fx.Source.Watch(ctx, pid, func() { fired <- struct{}{} }); err != nil {
			t.Fatalf("Watch: %v", err)
		}
		fx.KillChild(t, pid)

		select {
		case <-fired:
		case <-time.After(3 * time.Second):
			t.Fatal("onDeath did not fire within 3s")
		}
		select {
		case <-fired:
			t.Fatal("onDeath fired more than once")
		case <-time.After(300 * time.Millisecond):
		}
	})
}

// ---------------------------------------------------------------------------
// Seam 3 — window manager
// ---------------------------------------------------------------------------

// Window is the neutral client record. Address is an opaque, backend-owned ref
// (Hyprland 0x…, sway con_id, X11 window id) that consumers never parse.
type Window struct {
	Address   string
	PID       int
	Title     string
	Workspace string
}

// Manager is the window-manager seam.
type Manager interface {
	// Available reports — cheaply, without side effects — whether this backend's
	// WM is running. Returns false (never panics/hangs) when it is not.
	Available() bool
	Clients(ctx context.Context) ([]Window, error)
	ActiveWindow(ctx context.Context) (string, error)
	// Focus moves focus to ref; an invalid ref must error.
	Focus(ctx context.Context, ref string) error

	// NormalizeEventAddress converts a raw event-stream address into the form
	// returned by Clients (the seam owns this so backends produce already
	// comparable refs — pins the Hyprland 0x-prefix quirk).
	NormalizeEventAddress(raw string) string
	// TranslateEvent maps a raw event name to its canonical neutral name and
	// whether the seam handles it.
	TranslateEvent(rawName string) (canonical string, handled bool)
	// CanonicalEvents lists the canonical event names the seam emits.
	CanonicalEvents() []string
	// RawForm is the inverse of normalization for a Clients address — used only
	// by the suite to synthesize the event-stream form of a known address.
	RawForm(address string) string
}

// RunManagerContract asserts the neutral WM contract. The event-translation and
// normalization assertions run anywhere; the client/focus assertions need a
// live WM and skip when Available() is false.
func RunManagerContract(t *testing.T, m Manager) {
	t.Helper()

	t.Run("translate event maps canonical names and rejects unknowns", func(t *testing.T) {
		for _, name := range m.CanonicalEvents() {
			got, handled := m.TranslateEvent(name)
			if !handled {
				t.Errorf("canonical event %q reported not handled", name)
			}
			if got != name {
				t.Errorf("TranslateEvent(%q) = %q, want %q", name, got, name)
			}
		}
		if _, handled := m.TranslateEvent("definitely-not-an-event"); handled {
			t.Error("unknown event reported as handled")
		}
	})

	// ⚠ Address-normalization symmetry — the single most fragile cross-layer
	// contract, asserted purely (no live WM) so it runs in CI: an address in the
	// Clients form survives a round-trip through the event-stream raw form. The
	// seam owns this so non-Hyprland backends produce already-comparable refs.
	t.Run("event-address normalization round-trips", func(t *testing.T) {
		const addr = "0xdeadbeef"
		if got := m.NormalizeEventAddress(m.RawForm(addr)); got != addr {
			t.Errorf("NormalizeEventAddress(RawForm(%q)) = %q, want %q", addr, got, addr)
		}
	})

	if !liveConformance() {
		t.Log("live WM assertions gated off (set SWITCHBOARD_LIVE_CONFORMANCE=1 on a host with the backend)")
		return
	}
	if !m.Available() {
		t.Log("WM unavailable: skipping live client assertions (Available() reported false cleanly)")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	clients, err := m.Clients(ctx)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	for _, w := range clients {
		if w.Address == "" {
			t.Errorf("client has empty address: %+v", w)
		}
	}

	active, err := m.ActiveWindow(ctx)
	if err != nil {
		t.Fatalf("ActiveWindow: %v", err)
	}
	if active == "" {
		t.Skip("no active window to anchor the normalization/membership assertions")
	}

	// The active window is one of the clients.
	inClients := false
	for _, w := range clients {
		if w.Address == active {
			inClients = true
			break
		}
	}
	if !inClients {
		t.Errorf("active window %q not present in Clients()", active)
	}

	// ⚠ Address normalization (the single most fragile cross-layer contract):
	// the event-stream form of the active address normalizes back to exactly the
	// Clients() address.
	raw := m.RawForm(active)
	if got := m.NormalizeEventAddress(raw); got != active {
		t.Errorf("NormalizeEventAddress(%q) = %q, want %q (Clients form)", raw, got, active)
	}

	// Focus is intentionally NOT exercised here: on a live WM the success path
	// would steal the user's focus, and backends report invalid-ref failures
	// inconsistently (Hyprland often answers "ok" for a non-existent address).
	// Focus correctness is covered by integration tests of the rpc.focus path.
}

// ---------------------------------------------------------------------------
// Seam 2 — terminal locator
// ---------------------------------------------------------------------------

// Pane is the neutral pane record with a stable (mux, pane) identity.
type Pane struct {
	Mux         int
	PaneID      int
	TTY         string
	WindowTitle string
}

// Locator is the terminal seam.
type Locator interface {
	// Available reports cheaply whether this terminal backend is present.
	Available() bool
	// Locate returns the pane attached to tty, or (nil, nil) when none owns it.
	Locate(ctx context.Context, tty string) (*Pane, error)
	// SomeTTY returns a tty known to be owned by a live pane and true, or false
	// when the backend exposes no panes (live-portion fixtures only).
	SomeTTY(ctx context.Context) (string, bool)
}

// RunLocatorContract asserts the neutral terminal-locator contract. The
// unknown-tty path runs anywhere; the owned-tty path needs a live terminal and
// skips when Available() is false.
func RunLocatorContract(t *testing.T, l Locator) {
	t.Helper()

	t.Run("unknown tty resolves to no pane without error or hang", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		pane, err := l.Locate(ctx, "/dev/pts/this-is-not-a-real-tty")
		if err != nil {
			t.Errorf("Locate(unknown) err = %v, want nil", err)
		}
		if pane != nil {
			t.Errorf("Locate(unknown) = %+v, want nil", pane)
		}
	})

	if !liveConformance() {
		t.Log("live terminal assertions gated off (set SWITCHBOARD_LIVE_CONFORMANCE=1 on a host with the backend)")
		return
	}
	if !l.Available() {
		t.Log("terminal backend unavailable: skipping owned-tty assertion (Available() reported false cleanly)")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tty, ok := l.SomeTTY(ctx)
	if !ok {
		t.Skip("terminal backend exposes no panes to anchor the owned-tty assertion")
	}
	pane, err := l.Locate(ctx, tty)
	if err != nil {
		t.Fatalf("Locate(owned tty %q): %v", tty, err)
	}
	if pane == nil {
		t.Fatalf("Locate(owned tty %q) = nil, want a pane", tty)
	}
	if pane.TTY != tty {
		t.Errorf("located pane tty = %q, want %q", pane.TTY, tty)
	}
	if pane.Mux == 0 && pane.PaneID == 0 {
		t.Errorf("located pane lacks a stable (mux, pane) identity: %+v", pane)
	}
}
