package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain redirects HOME and XDG_CONFIG_HOME for the whole package so no test
// reads the developer's real ~/.claude/sessions/<pid>.json via label.RawName.
//
// TestObserveLabelEmitsOnChangeOnly is the case that named this: it renders pid
// 424242 and its own comment concedes it depends on no session file existing at
// that pid. That is a bet on the developer's process table, and it is the class
// of gap that costs an afternoon the one time it fires.
//
// See the sibling comment in cmd/switchboard-waybar for why this is package-level
// rather than one t.Setenv per test. Tests wanting a writable HOME of their own
// (fanout_test.go's rename case) still call t.Setenv and override this.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "switchboard-home")
	if err != nil {
		panic("hermetic test HOME: " + err.Error())
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	code := m.Run()

	os.RemoveAll(home) // explicit: os.Exit runs no deferred functions
	os.Exit(code)
}
