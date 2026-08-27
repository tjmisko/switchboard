package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// applySession seeds one session at the given CWD. Re-applying the same CWD
// reproduces the reconciler's every-5s no-op Apply, which is the case the
// suppressed counter has to see.
func applySession(store *state.Store, cwd string) {
	store.Apply(func(m map[int]*state.Session) {
		m[1] = &state.Session{PID: 1, CWD: cwd, TTY: "/dev/pts/1", StartedAt: time.Unix(1000, 0)}
	})
}

// The publish half of the ratio the line exists to report. A first Apply always
// changes the world (there is no published reference yet), so it must be counted
// as a publish and must not touch suppressed.
func TestSamplePublishStats_countsThePublishWhenApplyChangesTheSnapshot(t *testing.T) {
	store := state.New("")

	applySession(store, "/w")

	got := store.SamplePublishStats()
	if got.Publishes != 1 {
		t.Errorf("Publishes = %d, want 1", got.Publishes)
	}
	if got.Suppressed != 0 {
		t.Errorf("Suppressed = %d, want 0 (nothing was suppressed)", got.Suppressed)
	}
}

// The suppress half. The second Apply writes byte-identical state, so the change
// key matches and the publish is dropped — the exact event this counter is here
// to make visible, because from outside the process a suppressed Apply is
// indistinguishable from an Apply that never happened.
func TestSamplePublishStats_countsTheSuppressionWhenApplyRepeatsTheSnapshot(t *testing.T) {
	store := state.New("")

	applySession(store, "/w")
	applySession(store, "/w")
	applySession(store, "/w")

	got := store.SamplePublishStats()
	if got.Publishes != 1 {
		t.Errorf("Publishes = %d, want 1 (only the first Apply changed anything)", got.Publishes)
	}
	if got.Suppressed != 2 {
		t.Errorf("Suppressed = %d, want 2", got.Suppressed)
	}
}

// Both counters in one window, so a reader can see the ratio the change gate is
// judged by rather than two numbers from two windows.
func TestSamplePublishStats_countsPublishesAndSuppressionsInTheSameWindow(t *testing.T) {
	store := state.New("")

	applySession(store, "/a")
	applySession(store, "/a")
	applySession(store, "/b")
	applySession(store, "/b")
	applySession(store, "/b")

	got := store.SamplePublishStats()
	if got.Publishes != 2 || got.Suppressed != 3 {
		t.Errorf("Publishes/Suppressed = %d/%d, want 2/3", got.Publishes, got.Suppressed)
	}
}

// The reset half of the reset-vs-delta decision: a sample closes its window, so the
// next one reports only what happened after it. A daemon idle for a minute must
// report zeros, not the previous minute's totals again.
func TestSamplePublishStats_reportsZeroCountsWhenNothingAppliedSinceTheLastSample(t *testing.T) {
	store := state.New("")

	applySession(store, "/w")
	applySession(store, "/w")
	if first := store.SamplePublishStats(); first.Publishes != 1 || first.Suppressed != 1 {
		t.Fatalf("first window = %d/%d, want 1/1", first.Publishes, first.Suppressed)
	}

	got := store.SamplePublishStats()
	if got.Publishes != 0 || got.Suppressed != 0 {
		t.Errorf("second window = %d/%d, want 0/0; the counters must reset on sample, not accumulate",
			got.Publishes, got.Suppressed)
	}
	if got.Frames != 0 || got.FrameBytes != 0 {
		t.Errorf("second window frames = %d (%d bytes), want 0", got.Frames, got.FrameBytes)
	}
}

// Subscribers is an instantaneous reading, and it is the number that says
// whether a publish costs one encode and N wakeups or nothing at all.
func TestSamplePublishStats_reportsTheCurrentSubscriberCount(t *testing.T) {
	store := state.New("")
	if got := store.SamplePublishStats(); got.Subscribers != 0 {
		t.Fatalf("Subscribers = %d on a fresh store, want 0", got.Subscribers)
	}

	_, cancelA := store.Subscribe()
	_, cancelB := store.Subscribe()
	if got := store.SamplePublishStats(); got.Subscribers != 2 {
		t.Errorf("Subscribers = %d, want 2", got.Subscribers)
	}

	cancelA()
	cancelB()
	if got := store.SamplePublishStats(); got.Subscribers != 0 {
		t.Errorf("Subscribers = %d after both cancels, want 0", got.Subscribers)
	}
}

// frame_bytes is the mean over frames ACTUALLY ENCODED. broadcast returns before
// the encode when nobody is subscribed, so a publish on a bar-less daemon
// contributes a publish and no frame — and dividing by publishes would report a
// frame size no subscriber ever received.
func TestSamplePublishStats_countsNoFrameWhenAPublishHasNoSubscribers(t *testing.T) {
	store := state.New("")

	applySession(store, "/w")

	got := store.SamplePublishStats()
	if got.Publishes != 1 {
		t.Fatalf("Publishes = %d, want 1", got.Publishes)
	}
	if got.Frames != 0 || got.FrameBytes != 0 {
		t.Errorf("Frames = %d (%d bytes), want 0: an unsubscribed publish is never encoded", got.Frames, got.FrameBytes)
	}
	if got.MeanFrameBytes() != 0 {
		t.Errorf("MeanFrameBytes = %d, want 0 with no frames", got.MeanFrameBytes())
	}
}

// With a subscriber the encode happens once and is shared, so the frame count is
// per publish and not per subscriber, and its size is the size of the bytes that
// went out.
func TestSamplePublishStats_averagesTheEncodedFrameWhenSubscribersArePresent(t *testing.T) {
	store := state.New("")
	chA, cancelA := store.Subscribe()
	defer cancelA()
	_, cancelB := store.Subscribe()
	defer cancelB()

	applySession(store, "/w")
	frame := recvBroadcast(t, chA)

	got := store.SamplePublishStats()
	if got.Frames != 1 {
		t.Errorf("Frames = %d, want 1: the snapshot is encoded once per publish and shared, not once per subscriber", got.Frames)
	}
	if got.MeanFrameBytes() != uint64(len(frame.JSON)) {
		t.Errorf("MeanFrameBytes = %d, want %d (the bytes the subscribers received)",
			got.MeanFrameBytes(), len(frame.JSON))
	}
}

// The mean is over the window's frames, not the last one.
func TestSamplePublishStats_meansOverEveryFrameInTheWindow(t *testing.T) {
	stats := state.PublishStats{Frames: 4, FrameBytes: 4000}
	if got := stats.MeanFrameBytes(); got != 1000 {
		t.Errorf("MeanFrameBytes = %d, want 1000", got)
	}
}

// The window is measured, not assumed from the ticker interval: a sample reports
// the time since the previous sample so a delayed tick cannot silently inflate
// what reads as a per-minute count.
func TestSamplePublishStats_reportsTheElapsedWindowSinceTheLastSample(t *testing.T) {
	store := state.New("")
	if first := store.SamplePublishStats(); first.Window <= 0 {
		t.Errorf("first Window = %v, want the elapsed time since the store was created", first.Window)
	}

	before := time.Now()
	applySession(store, "/w")
	got := store.SamplePublishStats()
	if got.Window <= 0 || got.Window > time.Since(before)+time.Second {
		t.Errorf("Window = %v, want a positive duration bounded by the wall time since the previous sample", got.Window)
	}
}

// The journal line is the deliverable; pin its field names and order. Anything
// parsing it (journalctl -g, docs/telemetry.md) reads these exact keys.
func TestPublishStatsLine_rendersEveryFieldInOrder(t *testing.T) {
	stats := state.PublishStats{
		Window: 60 * time.Second, Publishes: 3, Suppressed: 281, Subscribers: 11,
		Frames: 3, FrameBytes: 41100, HeapAllocMB: 5, HeapSysMB: 33, VmHWMMB: 51,
	}
	want := "publish-stats: publishes=3 suppressed=281 subscribers=11 frame_bytes=13700 heap_alloc_mb=5 heap_sys_mb=33 vm_hwm_mb=51 window=1m0s"
	if got := stats.Line(); got != want {
		t.Errorf("Line() =\n  %s\nwant\n  %s", got, want)
	}
}

// A quiet daemon must still log its zero line: silence on this line has to mean
// the daemon is gone, never that it is idle.
func TestPublishStatsLine_rendersZeroesForAWindowWithNoActivity(t *testing.T) {
	line := state.PublishStats{Window: time.Minute}.Line()
	for _, want := range []string{"publishes=0", "suppressed=0", "subscribers=0", "frame_bytes=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("Line() = %q, want it to contain %q", line, want)
		}
	}
}

// The daemon starts LogPublishStats with its own context; it must die with it.
// Run with -race, this also covers counting from Apply concurrently with a
// sample, which is the lock-order pair (s.mu -> statsMu) the counters are split
// out to keep safe.
func TestLogPublishStats_stopsWhenTheContextIsCancelled(t *testing.T) {
	store := state.New("")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		store.LogPublishStats(ctx, time.Millisecond)
	}()

	// Churn while the sampler ticks, so the race detector sees both paths.
	for i := 0; i < 200; i++ {
		applySession(store, "/w")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("LogPublishStats did not return after its context was cancelled")
	}
}

// A non-positive interval disables the loop outright rather than spinning on a
// zero ticker (which panics) — the guard a future flag or a zeroed config needs.
func TestLogPublishStats_returnsImmediatelyWhenTheIntervalIsNotPositive(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		state.New("").LogPublishStats(context.Background(), 0)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("LogPublishStats(interval=0) did not return")
	}
}
