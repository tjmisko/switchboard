package remotestate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/federation"
	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/state"
)

const (
	testQuiet = 6 * time.Second
	testHold  = 45 * time.Second
)

// holdHarness is a Manager wired to a fake clock with the hold enabled, plus
// the two observations every test here makes: what subscribers were told, and
// which hosts had their routes invalidated.
type holdHarness struct {
	t       *testing.T
	manager *Manager
	clock   *fakeClock
	updates <-chan map[string]state.Snapshot
	cancel  func()
	removed []string
}

func newHoldHarness(t *testing.T, configure func(*ManagerConfig)) *holdHarness {
	t.Helper()
	harness := &holdHarness{t: t, clock: newFakeClock(testNow)}
	config := ManagerConfig{
		Destinations:  []string{"build", "spare"},
		HoldFor:       testHold,
		QuietFor:      testQuiet,
		Clock:         harness.clock,
		OnHostRemoved: func(host string) { harness.removed = append(harness.removed, host) },
	}
	if configure != nil {
		configure(&config)
	}
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	harness.manager = manager
	harness.updates, harness.cancel = manager.Subscribe()
	t.Cleanup(harness.cancel)
	return harness
}

// drain returns every replacement map queued since the last call.
func (h *holdHarness) drain() []map[string]state.Snapshot {
	h.t.Helper()
	var got []map[string]state.Snapshot
	for {
		select {
		case update := <-h.updates:
			got = append(got, update)
		default:
			return got
		}
	}
}

func (h *holdHarness) mustAccept(destination string, frame Frame) {
	h.t.Helper()
	if err := h.manager.accept(destination, frame); err != nil {
		h.t.Fatalf("accept(%s): %v", destination, err)
	}
}

func staleRow(t *testing.T, view map[string]state.Snapshot, host string) state.Session {
	t.Helper()
	snapshot, ok := view[host]
	if !ok {
		t.Fatalf("host %q absent from view %+v", host, view)
	}
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("host %q sessions = %d, want 1", host, len(snapshot.Sessions))
	}
	return snapshot.Sessions[0]
}

func TestManagerHoldsRowsSilentlyThroughTheQuietWindow(t *testing.T) {
	harness := newHoldHarness(t, nil)
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.drain()

	if outcome := harness.manager.endContact("build", lossTransport); !outcome.Held {
		t.Fatal("transport loss did not hold the host's last observation")
	}
	if updates := harness.drain(); len(updates) != 0 {
		t.Fatalf("quiet-window disconnect published %d updates, want none", len(updates))
	}
	row := staleRow(t, harness.manager.Snapshot(), "buildbox")
	if row.Stale || row.LastContact != nil {
		t.Fatalf("row marked stale inside the quiet window: %+v", row)
	}

	// A reconnect one second before the stale edge must be invisible: that is
	// the entire point of the quiet window.
	harness.clock.Advance(testQuiet - time.Second)
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	if updates := harness.drain(); len(updates) != 0 {
		t.Fatalf("reconnect inside the quiet window published %d updates, want none", len(updates))
	}
	if row := staleRow(t, harness.manager.Snapshot(), "buildbox"); row.Stale {
		t.Fatal("reconnected row is marked stale")
	}
}

func TestManagerMarksHeldRowsStaleWhenTheQuietWindowElapses(t *testing.T) {
	harness := newHoldHarness(t, nil)
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.drain()
	lostAt := harness.clock.Now()
	harness.manager.endContact("build", lossTransport)

	harness.clock.Advance(testQuiet)
	updates := harness.drain()
	if len(updates) != 1 {
		t.Fatalf("stale edge published %d updates, want exactly 1", len(updates))
	}
	row := staleRow(t, updates[0], "buildbox")
	if !row.Stale {
		t.Fatalf("row not marked stale after the quiet window: %+v", row)
	}
	if row.LastContact == nil || !row.LastContact.Equal(lostAt) {
		t.Fatalf("last_contact = %v, want the client instant of the last frame %v", row.LastContact, lostAt)
	}
	// The stored observation must stay the peer's own word: a stamped copy that
	// leaked back into storage would make every later reconnect look like news.
	if stored := harness.manager.hosts["buildbox"].snapshot; stored.Sessions[0].Stale {
		t.Fatal("stale marker was written back onto the stored snapshot")
	}
}

func TestManagerDropsHeldHostAndInvalidatesRoutesWhenTheHoldExpires(t *testing.T) {
	harness := newHoldHarness(t, nil)
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.manager.endContact("build", lossTransport)
	harness.clock.Advance(testQuiet)
	harness.drain()

	harness.clock.Advance(testHold - testQuiet)
	updates := harness.drain()
	if len(updates) != 1 || len(updates[len(updates)-1]) != 0 {
		t.Fatalf("hold expiry published %+v, want one empty replacement", updates)
	}
	if len(harness.manager.Snapshot()) != 0 {
		t.Fatalf("host survived the hold: %+v", harness.manager.Snapshot())
	}
	if len(harness.removed) != 1 || harness.removed[0] != "buildbox" {
		t.Fatalf("route invalidation = %v, want [buildbox] exactly once", harness.removed)
	}
	if pending := harness.clock.pending(); pending != 0 {
		t.Fatalf("%d deadlines still armed after removal", pending)
	}
}

func TestManagerClearsStalenessWhenContactReturnsAfterTheStaleEdge(t *testing.T) {
	harness := newHoldHarness(t, nil)
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.manager.endContact("build", lossTransport)
	harness.clock.Advance(testQuiet)
	harness.drain()

	harness.clock.Advance(time.Second)
	// Identical state: the row must still be republished, because the STALENESS
	// changed even though nothing the peer said did.
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	updates := harness.drain()
	if len(updates) != 1 {
		t.Fatalf("reconnect after the stale edge published %d updates, want 1", len(updates))
	}
	row := staleRow(t, updates[0], "buildbox")
	if row.Stale || row.LastContact != nil {
		t.Fatalf("row still stale after reconnect: %+v", row)
	}
	// The countdown must restart from the new contact, not resume the old one.
	harness.manager.endContact("build", lossTransport)
	harness.clock.Advance(testHold - time.Second)
	if len(harness.manager.Snapshot()) != 1 {
		t.Fatal("hold deadline was not restarted by the reconnect")
	}
}

func TestManagerDoesNotRepublishAKeepaliveThatCarriesUnchangedState(t *testing.T) {
	harness := newHoldHarness(t, nil)
	frame := Frame{Host: "buildbox", Snapshot: testSnapshot(42), KeepaliveSeconds: 10}
	harness.mustAccept("build", frame)
	if len(harness.drain()) != 1 {
		t.Fatal("first frame was not published")
	}
	for i := 0; i < 5; i++ {
		harness.clock.Advance(10 * time.Second)
		harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42), KeepaliveSeconds: 10})
	}
	if updates := harness.drain(); len(updates) != 0 {
		t.Fatalf("keepalives republished unchanged state %d times", len(updates))
	}
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(43), KeepaliveSeconds: 10})
	if updates := harness.drain(); len(updates) != 1 {
		t.Fatalf("changed keepalive published %d updates, want 1", len(updates))
	}
}

func TestManagerMarksASilentButConnectedPeerStaleOnlyWhenItAdvertisedAKeepalive(t *testing.T) {
	harness := newHoldHarness(t, nil)
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42), KeepaliveSeconds: 10})
	// A peer that promises nothing must never be marked stale on silence alone:
	// an older remote is quiet exactly because nothing is happening.
	harness.mustAccept("spare", Frame{Host: "sparebox", Snapshot: testSnapshot(7)})
	harness.drain()

	harness.clock.Advance(silenceMultiple*10*time.Second - time.Second)
	if row := staleRow(t, harness.manager.Snapshot(), "buildbox"); row.Stale {
		t.Fatal("advertised peer marked stale before its silence deadline")
	}
	harness.clock.Advance(time.Second)
	if row := staleRow(t, harness.manager.Snapshot(), "buildbox"); !row.Stale {
		t.Fatal("advertised peer not marked stale after missing its keepalives")
	}
	if row := staleRow(t, harness.manager.Snapshot(), "sparebox"); row.Stale {
		t.Fatal("peer that advertised no keepalive was marked stale on silence")
	}
	// Silence is never grounds for removal; only SSH may declare the link dead.
	harness.clock.Advance(10 * testHold)
	if _, live := harness.manager.Snapshot()["buildbox"]; !live {
		t.Fatal("a silent but connected host was removed")
	}
}

func TestManagerDropsImmediatelyWhenThePeerClosesOut(t *testing.T) {
	var diagnostics []Diagnostic
	harness := newHoldHarness(t, func(config *ManagerConfig) {
		config.OnDiagnostic = func(diagnostic Diagnostic) { diagnostics = append(diagnostics, diagnostic) }
	})
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.drain()

	err := harness.manager.accept("build", Frame{Host: "buildbox", Closeout: &Closeout{Reason: CloseoutSignal}})
	if !errors.Is(err, ErrCloseout) {
		t.Fatalf("closeout frame error = %v, want ErrCloseout", err)
	}
	outcome := harness.manager.endContact("build", classifyLoss(err))
	if outcome.Held {
		t.Fatal("closeout was held instead of dropped")
	}
	if len(harness.manager.Snapshot()) != 0 {
		t.Fatalf("closeout left rows behind: %+v", harness.manager.Snapshot())
	}
	if harness.removed == nil {
		t.Fatal("closeout did not invalidate routes")
	}
	harness.manager.diagnoseAttempt("build", outcome, err, nil)
	if len(diagnostics) != 1 || diagnostics[0].Category != DiagnosticCloseout || diagnostics[0].Reason != CloseoutSignal {
		t.Fatalf("diagnostics = %+v, want one closeout/signal", diagnostics)
	}
}

func TestManagerDropsImmediatelyOnAProtocolFailureButHoldsOnATruncatedFrame(t *testing.T) {
	// A peer we cannot parse will still be unparseable after the reconnect, so
	// holding rows we can never refresh would be a lie with only a timer to end
	// it. A frame cut in half is the opposite: the link failed, not the peer.
	protocol := []error{ErrSchemaMismatch, ErrInvalidFrame, ErrFrameTooLarge, ErrDuplicateHost, ErrHostnameChanged, ErrLocalHostname}
	for _, readErr := range protocol {
		if got := classifyLoss(readErr); got != lossProtocol {
			t.Fatalf("classifyLoss(%v) = %v, want lossProtocol", readErr, got)
		}
	}
	for _, readErr := range []error{ErrTruncatedFrame, errors.New("read |0: file already closed"), nil} {
		if got := classifyLoss(readErr); got != lossTransport {
			t.Fatalf("classifyLoss(%v) = %v, want lossTransport", readErr, got)
		}
	}

	harness := newHoldHarness(t, nil)
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.drain()
	if outcome := harness.manager.endContact("build", lossProtocol); outcome.Held {
		t.Fatal("a protocol failure held its host's rows")
	}
	if len(harness.manager.Snapshot()) != 0 {
		t.Fatalf("protocol failure left rows behind: %+v", harness.manager.Snapshot())
	}
}

func TestManagerDoesNotRestartTheHoldWhenAReconnectFailsBeforeReadingAFrame(t *testing.T) {
	harness := newHoldHarness(t, nil)
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.manager.endContact("build", lossTransport)

	// Model a destination that reconnects and immediately EOFs, repeatedly. A
	// flapping link must not be able to hold rows forever without refreshing
	// them, so only a real frame may restart the countdown.
	for elapsed := time.Duration(0); elapsed < testHold; elapsed += 5 * time.Second {
		harness.clock.Advance(5 * time.Second)
		harness.manager.endContact("build", lossTransport)
	}
	if live := harness.manager.Snapshot(); len(live) != 0 {
		t.Fatalf("flapping destination held rows past the hold deadline: %+v", live)
	}
}

func TestManagerReconnectDuringRouteInvalidationKeepsTheHostAndRepublishes(t *testing.T) {
	// The hold moves removal from the owning worker onto a timer, so a reconnect
	// can now race an expiry that is already inside its route-invalidation
	// callback. The epoch fence must let the reconnect win, and the abandoned
	// removal must republish so the live-route projection restores what the
	// callback tore down.
	var harness *holdHarness
	reentered := false
	harness = newHoldHarness(t, func(config *ManagerConfig) {
		config.OnHostRemoved = func(host string) {
			harness.removed = append(harness.removed, host)
			if reentered {
				return
			}
			reentered = true
			harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(99)})
		}
	})
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.manager.endContact("build", lossTransport)
	harness.clock.Advance(testHold)
	updates := harness.drain()

	live := harness.manager.Snapshot()
	if len(live) != 1 {
		t.Fatalf("reconnect lost the race with its own expiry: %+v", live)
	}
	if got := staleRow(t, live, "buildbox"); got.PID != 99 || got.Stale {
		t.Fatalf("surviving row = %+v, want the fresh pid 99, not stale", got)
	}
	if len(updates) == 0 {
		t.Fatal("abandoned removal published nothing; route liveness would stay torn down")
	}
	if final := updates[len(updates)-1]; len(final) != 1 {
		t.Fatalf("last replacement = %+v, want the re-adopted host", final)
	}
}

func TestManagerStopsEveryDeadlineWhenRunReturns(t *testing.T) {
	harness := newHoldHarness(t, func(config *ManagerConfig) {
		config.Destinations = nil
	})
	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)})
	harness.manager.endContact("build", lossTransport)
	if harness.clock.pending() == 0 {
		t.Fatal("no deadline was armed by the hold")
	}
	if err := harness.manager.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending := harness.clock.pending(); pending != 0 {
		t.Fatalf("%d deadlines survived shutdown", pending)
	}
	// Nothing reads held rows after shutdown, but firing route invalidation into
	// a view that is itself being torn down would be a shutdown-ordering hazard.
	harness.removed = nil
	harness.clock.Advance(10 * testHold)
	if harness.removed != nil {
		t.Fatalf("route invalidation fired after shutdown: %v", harness.removed)
	}
}

func TestNewManagerRejectsAnUnboundedHold(t *testing.T) {
	if _, err := NewManager(ManagerConfig{HoldFor: MaxHoldFor + time.Nanosecond}); err == nil {
		t.Fatal("NewManager accepted a hold beyond MaxHoldFor")
	}
	if _, err := NewManager(ManagerConfig{HoldFor: -time.Second}); err == nil {
		t.Fatal("NewManager accepted a negative hold")
	}
	if _, err := NewManager(ManagerConfig{QuietFor: -time.Second}); err == nil {
		t.Fatal("NewManager accepted a negative quiet window")
	}
}

func TestHeldHostKeepsItsRoutesLiveAndReachesTheAggregateMarkedStale(t *testing.T) {
	// The hold is only worth having if the row stays USABLE. What a remote chip
	// focuses is the local terminal pane displaying that SSH session, and losing
	// the state stream says nothing about where that window is — so the route
	// must survive the disconnect that the row survives, and the aggregate must
	// carry the staleness through to the renderer.
	harness := newHoldHarness(t, nil)
	registry := panebind.NewRegistry()
	harness.manager.onHostRemoved = registry.DropLiveHost

	snapshot := testSnapshot(42)
	key := panebind.ExactSessionKey{
		Hostname:  "buildbox",
		PID:       snapshot.Sessions[0].PID,
		StartedAt: snapshot.Sessions[0].StartedAt,
	}
	if err := registry.Bind(key, panebind.LocalPaneRef{GUIPID: 10, WindowID: 20, PaneID: 30}); err != nil {
		t.Fatal(err)
	}

	ctx, cancelRoutes := context.WithCancel(t.Context())
	defer cancelRoutes()
	changed := make(chan struct{}, 16)
	ready := make(chan struct{})
	go func() {
		_ = federation.RunLiveRoutesReady(ctx, harness.manager, registry, nil, func() { changed <- struct{}{} }, ready)
	}()
	<-ready
	<-changed

	harness.mustAccept("build", Frame{Host: "buildbox", Snapshot: snapshot})
	<-changed
	if _, err := registry.Resolve(key); err != nil {
		t.Fatalf("route was not live before the disconnect: %v", err)
	}

	harness.manager.endContact("build", lossTransport)
	harness.clock.Advance(testQuiet)
	<-changed
	if _, err := registry.Resolve(key); err != nil {
		t.Fatalf("held row lost its route: %v", err)
	}

	local := &state.Store{}
	view, err := federation.NewView(local, "clientbox", harness.manager)
	if err != nil {
		t.Fatal(err)
	}
	view.SetRouteReady(func(host string, pid int, startedAt time.Time) bool {
		_, err := registry.Resolve(panebind.ExactSessionKey{Hostname: host, PID: pid, StartedAt: startedAt})
		return err == nil
	})
	aggregate := view.Snapshot()
	if len(aggregate.Sessions) != 1 {
		t.Fatalf("aggregate sessions = %+v, want the held remote row", aggregate.Sessions)
	}
	row := aggregate.Sessions[0]
	if !row.Stale || row.LastContact == nil {
		t.Fatalf("aggregate row lost the staleness verdict: %+v", row)
	}
	if !row.Navigable {
		t.Fatal("held row is not navigable; the pane it focuses has not moved")
	}

	harness.clock.Advance(testHold - testQuiet)
	<-changed
	if _, err := registry.Resolve(key); !errors.Is(err, panebind.ErrSessionNotLive) {
		t.Fatalf("Resolve after the hold expired = %v, want ErrSessionNotLive", err)
	}
	if len(view.Snapshot().Sessions) != 0 {
		t.Fatalf("dropped host still in the aggregate: %+v", view.Snapshot().Sessions)
	}
}
