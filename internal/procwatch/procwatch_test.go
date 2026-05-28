package procwatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/procwatch"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

// §9 procwatch — seed of the death-semantics suite (0.6 expands it): onDeath
// fires exactly once when a real watched child is killed. Driven by the
// harness's real short-lived child.
func TestWatchFiresOnDeathExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	child := testsupport.SpawnSleep(t, 60*time.Second)

	fired := make(chan struct{}, 4)
	w := procwatch.New()
	if err := w.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	child.Kill(t)

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("onDeath did not fire within 3s of kill")
	}

	// It must not fire a second time.
	select {
	case <-fired:
		t.Fatal("onDeath fired more than once")
	case <-time.After(300 * time.Millisecond):
	}
}
