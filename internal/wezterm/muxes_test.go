package wezterm_test

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/testsupport"
	"github.com/tjmisko/switchboard/internal/wezterm"
)

// §8.1 Muxes — keeps only gui-sock-<pid> entries whose owning pid is alive,
// skipping the dead socket, the non-numeric suffix, and the unrelated file.
// Driven by the harness's fake $XDG_RUNTIME_DIR/wezterm layout.
func TestMuxesKeepsOnlyLiveGuiSockets(t *testing.T) {
	rt := testsupport.NewWeztermRuntime(t)
	rt.AddMux(t, testsupport.LivePID())   // survives — owning pid is alive
	rt.AddMux(t, testsupport.DeadPID())   // skipped — no /proc entry
	rt.AddEntry(t, "gui-sock-notanumber") // skipped — non-numeric suffix
	rt.AddEntry(t, "some-other-file")     // skipped — wrong prefix

	muxes, err := wezterm.Muxes()
	if err != nil {
		t.Fatalf("Muxes: %v", err)
	}
	if len(muxes) != 1 {
		t.Fatalf("got %d muxes, want 1: %+v", len(muxes), muxes)
	}
	if muxes[0].PID != testsupport.LivePID() {
		t.Errorf("mux PID = %d, want live pid %d", muxes[0].PID, testsupport.LivePID())
	}
}
