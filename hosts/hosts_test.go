// Package hosts_test pins the invariants of the per-host systemd overlays that
// scripts/deploy installs. An overlay is live configuration for a real machine
// but is otherwise unexercised by any build, so these are the only checks that
// a bad edit meets before it silently changes what a host runs.
package hosts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func overlayFiles(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	matches, err := filepath.Glob(filepath.Join(".", "*", "systemd", "*", "*.conf"))
	if err != nil {
		t.Fatalf("glob overlays: %v", err)
	}
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		found[path] = string(b)
	}
	return found
}

// Pointing a unit at a release directory defeats the atomic flip: the deploy
// moves `current` while the unit keeps launching the pinned revision — the
// exact silent no-op the release model exists to remove.
func TestOverlays_shouldResolveBinariesThroughCurrentWhenTheySelectOne(t *testing.T) {
	for path, text := range overlayFiles(t) {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if !strings.Contains(line, "SWITCHBOARD_BIN=") && !strings.Contains(line, "SWITCHBOARD_CTL=") {
				continue
			}
			if strings.Contains(line, "/releases/") {
				t.Errorf("%s pins a release directory, defeating the atomic flip:\n  %s", path, line)
			}
			if !strings.Contains(line, "/current/") {
				t.Errorf("%s selects a binary without resolving through current:\n  %s", path, line)
			}
		}
	}
}

// ~/go/bin is `go install` output, not a deployment. A host that points a unit
// there reintroduces the two-locations problem: the wrong one still runs, and
// the restart still reports active.
func TestOverlays_shouldNeverSelectTheGoInstallDirectory(t *testing.T) {
	for path, text := range overlayFiles(t) {
		if strings.Contains(text, "go/bin") {
			t.Errorf("%s selects ~/go/bin; deployments resolve through current", path)
		}
	}
}

// Regression guard for a real incident: this host's federation peer lived as
// an in-place edit to the shared unit, where the next `install -Dm644` would
// have reverted it without a word. It belongs in the overlay, and the overlay
// must keep carrying it.
func TestGoosebookOverlay_shouldCarryItsFederationPeer(t *testing.T) {
	path := filepath.Join("goosebook", "systemd", "switchboard.service.d", "20-machine.conf")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no goosebook overlay: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "SWITCHBOARD_ARGS=") {
		t.Error("goosebook overlay no longer sets SWITCHBOARD_ARGS; its federation peer would be lost on the next unit install")
	}
	if !strings.Contains(text, "-remote ") {
		t.Error("goosebook overlay sets no -remote peer")
	}
}

// systemd merges drop-ins in lexical filename order, so the same variable set
// by two files in one .d directory resolves by filename rather than intent.
func TestOverlays_shouldNotSetOneVariableFromTwoDropInsInTheSameDirectory(t *testing.T) {
	seen := map[string]string{}
	for path, text := range overlayFiles(t) {
		dir := filepath.Dir(path)
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "Environment=") {
				continue
			}
			assignment := strings.TrimPrefix(line, "Environment=")
			assignment = strings.TrimPrefix(assignment, `"`)
			name, _, ok := strings.Cut(assignment, "=")
			if !ok {
				continue
			}
			key := dir + "\x00" + name
			if other, dup := seen[key]; dup && other != path {
				t.Errorf("%s and %s both set %s in %s; the winner depends on filename order", other, path, name, dir)
			}
			seen[key] = path
		}
	}
}
