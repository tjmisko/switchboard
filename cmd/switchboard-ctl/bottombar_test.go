package main

import (
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
