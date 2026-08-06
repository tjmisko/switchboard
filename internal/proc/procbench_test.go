package proc

import (
	"os"
	"testing"
)

// What one session costs the reconcile tick per /proc read, so the decision to
// leave these inside the store lock (or not) rests on a number.
func BenchmarkState(b *testing.B) {
	pid := os.Getpid()
	for b.Loop() {
		if _, err := State(pid); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRead(b *testing.B) {
	pid := os.Getpid()
	for b.Loop() {
		if _, err := Read(pid); err != nil {
			b.Fatal(err)
		}
	}
}
