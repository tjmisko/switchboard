package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

// §10.1 shouldRun — the bottom-bar invariant's pure core. All four F8
// truth-table cases.
func TestShouldRun(t *testing.T) {
	tests := []struct {
		name       string
		topVisible bool
		count      int
		want       bool
	}{
		{"top hidden, no sessions", false, 0, false},
		{"top hidden, sessions present", false, 3, false},
		{"top visible, no sessions", true, 0, false},
		{"top visible, sessions present", true, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRun(tt.topVisible, tt.count); got != tt.want {
				t.Errorf("shouldRun(%v, %d) = %v, want %v", tt.topVisible, tt.count, got, tt.want)
			}
		})
	}
}

// §10.2 topVisible / bottomPID / envOr — seed cases (0.4 expands). Uses the
// harness's temp-file builders for the master-visibility marker and the stale
// pidfile.
func TestTopVisible(t *testing.T) {
	dir := t.TempDir()
	cfg := bottomBarConfig{marker: filepath.Join(dir, "waybar-hidden")}

	if !topVisible(cfg) {
		t.Error("topVisible should be true when the marker is absent")
	}
	testsupport.Touch(t, cfg.marker)
	if topVisible(cfg) {
		t.Error("topVisible should be false when the marker is present")
	}
}

func TestBottomPIDCleansStalePidfile(t *testing.T) {
	dir := t.TempDir()
	cfg := bottomBarConfig{pidFile: filepath.Join(dir, "bottom.pid")}

	// Our own pid is live but its comm is the test binary, not "waybar", so the
	// pid-reuse guard must reject it and remove the stale pidfile.
	testsupport.WritePIDFile(t, cfg.pidFile, os.Getpid())
	if got := bottomPID(cfg); got != 0 {
		t.Errorf("bottomPID = %d, want 0 (comm guard)", got)
	}
	if _, err := os.Stat(cfg.pidFile); !os.IsNotExist(err) {
		t.Error("stale pidfile was not cleaned up")
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("SWITCHBOARD_TEST_ENVOR", "set-value")
	if got := envOr("SWITCHBOARD_TEST_ENVOR", "fallback"); got != "set-value" {
		t.Errorf("envOr (set) = %q, want set-value", got)
	}
	if got := envOr("SWITCHBOARD_TEST_ENVOR_MISSING", "fallback"); got != "fallback" {
		t.Errorf("envOr (unset) = %q, want fallback", got)
	}
}

// stubOps is an in-memory bottomBarOps: it tracks a running flag and counts
// start/stop calls, so the reconcile orchestration can be driven without
// launching a real waybar.
type stubOps struct {
	running bool
	starts  int
	stops   int
}

func (s *stubOps) ops() bottomBarOps {
	return bottomBarOps{
		isRunning: func(bottomBarConfig) bool { return s.running },
		start:     func(bottomBarConfig) error { s.running = true; s.starts++; return nil },
		stop:      func(bottomBarConfig) { s.running = false; s.stops++ },
	}
}

func newStubConfig(t *testing.T, stub *stubOps) bottomBarConfig {
	dir := t.TempDir()
	return bottomBarConfig{
		marker:     filepath.Join(dir, "waybar-hidden"),
		lockFile:   filepath.Join(dir, "bottombar.lock"),
		socketPath: filepath.Join(dir, "nonexistent.sock"),
		ops:        stub.ops(),
	}
}

// §11 reconcileWith — the four F8 truth-table cases end-to-end (marker drives
// topVisible). (top-hidden, bottom-present) never arises: hidden always stops.
func TestReconcileWithF8TruthTable(t *testing.T) {
	cases := []struct {
		name        string
		topHidden   bool
		count       int
		wantRunning bool
	}{
		{"hidden, no sessions", true, 0, false},
		{"hidden, sessions present", true, 3, false},
		{"visible, no sessions", false, 0, false},
		{"visible, sessions present", false, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubOps{}
			cfg := newStubConfig(t, stub)
			if c.topHidden {
				testsupport.Touch(t, cfg.marker)
			}
			reconcileWith(cfg, c.count)
			if stub.running != c.wantRunning {
				t.Errorf("running = %v, want %v", stub.running, c.wantRunning)
			}
		})
	}
}

// §11 reconcile — top hidden stops the bar without dialing the daemon.
func TestReconcileTopHiddenStopsWithoutDaemon(t *testing.T) {
	stub := &stubOps{running: true}
	cfg := newStubConfig(t, stub)
	testsupport.Touch(t, cfg.marker) // hidden

	reconcile(cfg)

	if stub.running {
		t.Error("bottom bar should be stopped when top is hidden")
	}
	if stub.stops == 0 {
		t.Error("stop should have been called")
	}
}

// §11 reconcile — when the daemon is unreachable (top visible, no socket) the
// bar is left in whatever state it is, no flap.
func TestReconcileDaemonUnreachableLeavesAsIs(t *testing.T) {
	stub := &stubOps{running: true}
	cfg := newStubConfig(t, stub) // marker absent => top visible; socket does not exist

	reconcile(cfg)

	if !stub.running {
		t.Error("running bar must be left as-is when the daemon is unreachable")
	}
	if stub.starts != 0 || stub.stops != 0 {
		t.Errorf("no start/stop expected (got starts=%d stops=%d)", stub.starts, stub.stops)
	}
}

// §11 ensureStarted/ensureStopped idempotence.
func TestEnsureStartStopIdempotent(t *testing.T) {
	stub := &stubOps{}
	cfg := bottomBarConfig{ops: stub.ops()}

	ensureStarted(cfg)
	ensureStarted(cfg) // already running -> no second launch
	if stub.starts != 1 {
		t.Errorf("start called %d times, want 1 (idempotent)", stub.starts)
	}

	ensureStopped(cfg)
	if stub.running {
		t.Error("bar should be stopped")
	}
	ensureStopped(cfg) // already stopped -> no error/panic
}

func TestRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := runtimeDir(); got != "/run/user/1000" {
		t.Errorf("runtimeDir (set) = %q, want /run/user/1000", got)
	}

	t.Setenv("XDG_RUNTIME_DIR", "")
	got := runtimeDir()
	want := fmt.Sprintf("/tmp/run-%d", os.Getuid())
	if got != want {
		t.Errorf("runtimeDir (unset) = %q, want %q", got, want)
	}
}
