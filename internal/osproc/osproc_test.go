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

// #97: the birth token rides the same projection, so the rpc hook walk and
// every Source caller see it without a second read.
func TestFromProcShouldCarryTheBirthTokenWhenTheReadHadOne(t *testing.T) {
	got := FromProc(proc.Info{PID: 7, Comm: "pi", Birth: "boot:42"})
	if got.Birth != "boot:42" || got.Lifetime() != (Lifetime{PID: 7, Birth: "boot:42"}) {
		t.Fatalf("FromProc = %+v, want the birth token carried", got)
	}
}

// An empty token on either side is unverified, which is never a match.
func TestCompareBirthShouldNeverMatchWhenEitherTokenIsMissing(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want BirthMatch
	}{
		{"boot:1", "boot:1", BirthSame},
		{"boot:1", "boot:2", BirthDifferent},
		{"", "boot:1", BirthUnverified},
		{"boot:1", "", BirthUnverified},
		{"", "", BirthUnverified},
	} {
		if got := CompareBirth(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareBirth(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
