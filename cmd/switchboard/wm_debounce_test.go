package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// testDebounce is short enough to keep these cases fast and long enough that a
// burst delivered on one channel send loop lands inside a single window.
const testDebounce = 40 * time.Millisecond

// countingLocator owns no pane and counts how many times it was asked. Reconcile
// calls Locate first and returns as soon as it yields no pane, so the count is
// exactly the number of per-session re-resolves the debounce let through — which
// is the quantity this whole change exists to reduce.
type countingLocator struct {
	mu sync.Mutex
	n  int
}

func (l *countingLocator) Name() string    { return "counting" }
func (l *countingLocator) Available() bool { return true }

func (l *countingLocator) Locate(context.Context, string) (*terminal.PaneRef, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return nil, nil
}

func (l *countingLocator) Activate(context.Context, *terminal.PaneRef) error { return nil }

func (l *countingLocator) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// stubManager satisfies wm.Manager without a live compositor. Reconcile never
// reaches it while countingLocator owns no pane, but a Resolver must be built
// over something, and a stub fails loudly rather than nil-panicking if the
// resolve order ever changes.
type stubManager struct{}

func (stubManager) Name() string                                 { return "stub" }
func (stubManager) Available() bool                              { return true }
func (stubManager) Clients(context.Context) ([]wm.Window, error) { return nil, nil }
func (stubManager) ActiveWindow(context.Context) (string, error) { return "", nil }
func (stubManager) Focus(context.Context, string) error          { return nil }
func (stubManager) Subscribe(context.Context) (<-chan wm.Event, error) {
	return nil, nil
}

// debounceHarness wires a store holding one resolvable session to a drainWMEvents
// goroutine, and hands back the event channel plus the locator's call counter.
func debounceHarness(t *testing.T) (*state.Store, chan wm.Event, *countingLocator, func()) {
	t.Helper()
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[4242] = &state.Session{
			PID:       4242,
			TTY:       "/dev/pts/7",
			CWD:       "/home/test",
			StartedAt: time.Now(),
			Hyprland:  &state.HyprlandInfo{Address: "0xdead"},
		}
	})
	loc := &countingLocator{}
	resolver := mapping.NewResolver(loc, stubManager{})
	events := make(chan wm.Event)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		drainWMEvents(ctx, store, resolver, events, nil, nil, nil, nil, testDebounce)
	}()
	return store, events, loc, func() {
		cancel()
		<-done
	}
}

func TestShouldCoalesceALayoutBurstIntoOneReresolveWhenEventsArriveTogether(t *testing.T) {
	_, events, loc, stop := debounceHarness(t)
	defer stop()

	for range 10 {
		events <- wm.Event{Kind: wm.EventLayoutChanged}
	}
	// Past the trailing edge, with margin for a loaded scheduler.
	time.Sleep(testDebounce * 5)

	if got := loc.count(); got != 1 {
		t.Errorf("re-resolved %d times after a 10-event burst, want exactly 1", got)
	}
}

func TestShouldReresolveAgainWhenASecondBurstArrivesAfterTheWindow(t *testing.T) {
	_, events, loc, stop := debounceHarness(t)
	defer stop()

	events <- wm.Event{Kind: wm.EventLayoutChanged}
	time.Sleep(testDebounce * 5)
	events <- wm.Event{Kind: wm.EventLayoutChanged}
	time.Sleep(testDebounce * 5)

	// Coalescing must not degrade into "only ever fire once" — separated bursts
	// are separate world changes and each has to land.
	if got := loc.count(); got != 2 {
		t.Errorf("re-resolved %d times across two separated bursts, want 2", got)
	}
}

func TestShouldDispatchAFocusEventImmediatelyWhenALayoutBurstIsPending(t *testing.T) {
	store, events, _, stop := debounceHarness(t)
	defer stop()

	events <- wm.Event{Kind: wm.EventLayoutChanged}
	events <- wm.Event{Kind: wm.EventFocusChanged, Address: "0xdead"}

	// Well inside the debounce window: focus must NOT be queued behind it, because
	// the chip highlight and the click-to-focus round trip both ride on it.
	deadline := time.Now().Add(testDebounce / 2)
	for time.Now().Before(deadline) {
		snap := store.Snapshot()
		if len(snap.Sessions) == 1 && snap.Sessions[0].Focused {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("focus did not land within the debounce window; it was coalesced with layout")
}

func TestShouldFlushAPendingCoalesceWhenTheEventStreamCloses(t *testing.T) {
	_, events, loc, stop := debounceHarness(t)
	defer stop()

	events <- wm.Event{Kind: wm.EventLayoutChanged}
	// Close well inside the window: the burst's last state must survive the
	// disconnect rather than waiting on the next reconcile tick to rediscover it.
	close(events)
	time.Sleep(testDebounce * 5)

	if got := loc.count(); got != 1 {
		t.Errorf("re-resolved %d times after the stream closed mid-window, want 1", got)
	}
}

// The three cases below pin the findings an adversarial review of the original
// debounce turned up. None of the four tests above can catch any of them: they
// exercise bursts that STOP, and every defect here needs a burst that does not.

func TestShouldStillReresolveWhenLayoutEventsNeverStopArriving(t *testing.T) {
	_, events, loc, stop := debounceHarness(t)
	defer stop()

	// Events spaced well inside the window, sustained for several windows. A
	// trailing-edge debounce that re-arms on every event has NO maximum wait: the
	// timer is reset before it can fire and the layout path re-resolves ZERO times
	// for as long as the stream keeps up. That is the shape a terminal repainting
	// its window title at animation rate produces during a turn.
	// The assertion MUST be made while events are still flowing. Letting the burst
	// stop first defeats the test entirely: the final Reset then fires during the
	// quiet period and the buggy code looks correct. (It did, on the first draft
	// of this test.)
	sending := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-sending:
				return
			case events <- wm.Event{Kind: wm.EventLayoutChanged}:
				time.Sleep(testDebounce / 8)
			}
		}
	}()

	time.Sleep(testDebounce * 4)
	got := loc.count()
	close(sending)
	<-done

	if got == 0 {
		t.Error("no re-resolve happened across four debounce windows of continuous layout events; " +
			"the timer is being re-armed before it can fire, so staleness is bounded by the " +
			"reconcile interval rather than the debounce window")
	}
}

func TestShouldLandAPendingLayoutReresolveBeforeDispatchingAFocusEvent(t *testing.T) {
	store, events, loc, stop := debounceHarness(t)
	defer stop()

	events <- wm.Event{Kind: wm.EventLayoutChanged}
	events <- wm.Event{Kind: wm.EventFocusChanged, Address: "0xdead"}

	// The focus event must have been preceded by the pending re-resolve. Ordering
	// is what the old `for evt := range events` loop gave for free, and it is
	// load-bearing: every wezterm window shares one mux pid, so a layout event
	// carrying a title change is often the only thing that disambiguates a session
	// well enough for applyFocus to match its address. Let focus overtake it and
	// applyFocus finds no holder, unfocuses everything, and records a focus edge
	// with an empty session id.
	deadline := time.Now().Add(testDebounce / 2)
	for time.Now().Before(deadline) {
		if loc.count() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	snap := store.Snapshot()
	t.Errorf("focus was dispatched without first landing the pending layout re-resolve "+
		"(re-resolves=%d, sessions=%+v)", loc.count(), snap.Sessions)
}
