package osproc

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/proc"
)

// discovery.IsPi gates on StdinTTY, and the rpc hook walk reaches discovery
// through FromProc, so a field dropped here would silently stop Pi discovery.
func TestFromProcShouldCarryStdinTTYWhenFDZeroIsTheTerminal(t *testing.T) {
	got := FromProc(proc.Info{PID: 7, Comm: "pi", TTY: "/dev/pts/3", StdinTTY: true})
	if !got.StdinTTY || got.TTY != "/dev/pts/3" {
		t.Fatalf("FromProc = %+v, want StdinTTY and TTY carried", got)
	}
}
