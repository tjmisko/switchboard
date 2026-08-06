package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain redirects HOME and XDG_CONFIG_HOME for the whole package so no test
// can reach the developer's real ~/.claude/sessions/<pid>.json (label.RawName)
// or ~/.config/switchboard/projects.json (projectname.ConfigPath).
//
// Package-level rather than one t.Setenv per test, because the per-test form has
// already rotted once here. The gap was written up naming three tests; by the
// time it was closed the blocked-writer work had added four more with the same
// shape, all rendering pid 4821. Those tests pass only because no session file
// happens to exist at that pid on this machine — a developer whose pid 4821 IS a
// live Claude session gets a mystery failure. A default that new tests inherit
// is the only version of this fix that cannot be forgotten by the next test.
//
// XDG_CONFIG_HOME has to move too: ConfigPath prefers it over $HOME, so setting
// HOME alone would still read the real projects.json wherever it is set.
//
// Tests that need a writable HOME of their own (newNameConfigFixture, the
// rename cases, the benchmarks) still call t.Setenv and override this.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "switchboard-waybar-home")
	if err != nil {
		panic("hermetic test HOME: " + err.Error())
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	code := m.Run()

	os.RemoveAll(home) // explicit: os.Exit runs no deferred functions
	os.Exit(code)
}
