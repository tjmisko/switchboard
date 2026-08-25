package panebind

import (
	"errors"
	"sync"
	"testing"
)

func TestBindingBeforeSnapshotIsCandidateButNotNavigable(t *testing.T) {
	r := NewRegistry()
	key := exact("buildbox", 11, "2026-08-24T20:00:00Z")
	pane := LocalPaneRef{GUIPID: 100, WindowID: 2, PaneID: 3}
	if err := r.Bind(key, pane); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(key); !errors.Is(err, ErrSessionNotLive) {
		t.Fatalf("pre-live Resolve error = %v", err)
	}
	if got, bound, live := r.SessionForPane(pane); !bound || live || !got.Equal(key) {
		t.Fatalf("inverse = (%+v,%t,%t)", got, bound, live)
	}
	if err := r.ReplaceLive("buildbox", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(key); err != nil || got != pane {
		t.Fatalf("Resolve = (%+v,%v), want (%+v,nil)", got, err, pane)
	}
}

func TestNeverLiveCandidateSurvivesOmittingSnapshotThenBecomesLive(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	if err := r.Bind(key, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", nil); err != nil {
		t.Fatal(err)
	}
	if got, bound, live := r.SessionForPane(pane); !bound || live || !got.Equal(key) {
		t.Fatalf("pre-snapshot candidate = (%+v,%t,%t)", got, bound, live)
	}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(key); err != nil || got != pane {
		t.Fatalf("candidate did not become live: (%+v,%v)", got, err)
	}
}

func TestPreviouslyLiveCandidateIsPrunedWhenCompleteSnapshotOmitsIt(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	if err := r.Bind(key, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", nil); err != nil {
		t.Fatal(err)
	}
	if _, bound, live := r.SessionForPane(pane); bound || live {
		t.Fatalf("dead candidate remains: bound=%t live=%t", bound, live)
	}
}

func TestSameKeyReannouncementPreservesSeenLivePruning(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	before := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	after := LocalPaneRef{GUIPID: 10, WindowID: 9, PaneID: 7}
	if err := r.Bind(key, before); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(key, after); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", nil); err != nil {
		t.Fatal(err)
	}
	if _, bound, _ := r.SessionForPane(after); bound {
		t.Fatal("same-key reannouncement reset seen-live state")
	}
}

func TestConcurrentReannouncementAndLiveReplacementRemainConsistent(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Bind(key, pane)
				_ = r.ReplaceLive("h", []ExactSessionKey{key})
				_ = r.ReplaceLive("h", nil)
			}
		}()
	}
	wg.Wait()
	// Establish a deterministic final seen-live lifecycle after the race.
	if err := r.Bind(key, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", nil); err != nil {
		t.Fatal(err)
	}
	if _, bound, _ := r.SessionForPane(pane); bound {
		t.Fatal("final omitted seen-live candidate remains bound")
	}
}

func TestNewBindingForPaneReplacesItsPreviousSession(t *testing.T) {
	r := NewRegistry()
	oldKey := exact("h", 1, "2026-08-24T20:00:00Z")
	newKey := exact("h", 2, "2026-08-24T20:00:01Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 0, PaneID: 0}
	if err := r.ReplaceLive("h", []ExactSessionKey{oldKey, newKey}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(oldKey, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(newKey, pane); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(oldKey); !errors.Is(err, ErrSessionUnbound) {
		t.Fatalf("old Resolve error = %v", err)
	}
	if got, err := r.Resolve(newKey); err != nil || got != pane {
		t.Fatalf("new Resolve = (%+v,%v)", got, err)
	}
}

func TestBindReplacingReturnsExactPriorSessionIncludingReannouncement(t *testing.T) {
	r := NewRegistry()
	first := exact("h", 1, "2026-08-24T20:00:00Z")
	second := exact("h", 2, "2026-08-24T20:00:01Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	if result, err := r.BindReplacing(first, pane); err != nil || result.HadPrevious || !result.Changed {
		t.Fatalf("initial BindReplacing = (%+v,%v)", result, err)
	}
	if result, err := r.BindReplacing(second, pane); err != nil || !result.HadPrevious || !result.Changed ||
		!result.PreviousSession.Equal(first) || result.PreviousRef != pane {
		t.Fatalf("replacement result = (%+v,%v), want first/pane,true,nil", result, err)
	}
	if result, err := r.BindReplacing(second, pane); err != nil || !result.HadPrevious || result.Changed ||
		!result.PreviousSession.Equal(second) || result.PreviousRef != pane {
		t.Fatalf("reannouncement result = (%+v,%v), want exact unchanged replay", result, err)
	}
	moved := pane
	moved.WindowID++
	if result, err := r.BindReplacing(second, moved); err != nil || !result.HadPrevious || !result.Changed ||
		!result.PreviousSession.Equal(second) || result.PreviousRef != pane {
		t.Fatalf("moved-ref result = (%+v,%v), want prior ref and changed", result, err)
	}
}

func TestConcurrentBindReplacingReturnsSerializedPriorChain(t *testing.T) {
	r := NewRegistry()
	first := exact("h", 1, "2026-08-24T20:00:00Z")
	second := exact("h", 2, "2026-08-24T20:00:01Z")
	third := exact("h", 3, "2026-08-24T20:00:02Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	if err := r.Bind(first, pane); err != nil {
		t.Fatal(err)
	}
	type result struct {
		installed ExactSessionKey
		replaced  BindingReplacement
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, key := range []ExactSessionKey{second, third} {
		go func() {
			<-start
			replaced, err := r.BindReplacing(key, pane)
			results <- result{installed: key, replaced: replaced, err: err}
		}()
	}
	close(start)
	one, two := <-results, <-results
	for _, got := range []result{one, two} {
		if got.err != nil || !got.replaced.HadPrevious || !got.replaced.Changed {
			t.Fatalf("concurrent result = %+v", got)
		}
	}
	var firstWriter, secondWriter result
	if one.replaced.PreviousSession.Equal(first) {
		firstWriter, secondWriter = one, two
	} else if two.replaced.PreviousSession.Equal(first) {
		firstWriter, secondWriter = two, one
	} else {
		t.Fatalf("neither writer observed initial prior: %+v %+v", one, two)
	}
	if !secondWriter.replaced.PreviousSession.Equal(firstWriter.installed) {
		t.Fatalf("prior chain is not serialized: first=%+v second=%+v", firstWriter, secondWriter)
	}
	if got, bound, _ := r.SessionForPane(pane); !bound || !got.Equal(secondWriter.installed) {
		t.Fatalf("final binding = (%+v,%t), want %+v", got, bound, secondWriter.installed)
	}
}

func TestMultiplePanesForOneExactSessionFailClosed(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	pane1 := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 1}
	pane2 := LocalPaneRef{GUIPID: 10, WindowID: 2, PaneID: 2}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(key, pane1); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(key, pane2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(key); !errors.Is(err, ErrSessionAmbiguous) {
		t.Fatalf("Resolve error = %v, want ambiguous", err)
	}
	if r.IsLiveRoute(key, pane1) || r.IsLiveRoute(key, pane2) {
		t.Fatal("an ambiguous route must not pass the final guard")
	}
	r.UnbindPane(pane1)
	if got, err := r.Resolve(key); err != nil || got != pane2 {
		t.Fatalf("Resolve after unbind = (%+v,%v)", got, err)
	}
}

func TestPIDReuseCannotUseOldExactBinding(t *testing.T) {
	r := NewRegistry()
	oldKey := exact("h", 42, "2026-08-24T20:00:00Z")
	newKey := exact("h", 42, "2026-08-24T21:00:00Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 1}
	if err := r.Bind(oldKey, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", []ExactSessionKey{newKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(oldKey); !errors.Is(err, ErrSessionNotLive) {
		t.Fatalf("old Resolve error = %v", err)
	}
	if _, err := r.Resolve(newKey); !errors.Is(err, ErrSessionUnbound) {
		t.Fatalf("new Resolve error = %v", err)
	}
}

func TestDropHostImmediatelyGatesExistingCandidate(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 1}
	if err := r.Bind(key, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	r.DropLiveHost("h")
	if _, err := r.Resolve(key); !errors.Is(err, ErrSessionNotLive) {
		t.Fatalf("Resolve error = %v", err)
	}
	if _, bound, live := r.SessionForPane(pane); bound || live {
		t.Fatalf("bound=%t live=%t, want false,false", bound, live)
	}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(key); !errors.Is(err, ErrSessionUnbound) {
		t.Fatalf("reconnect reused candidate: Resolve error = %v", err)
	}
}

func TestMovedPaneRefreshesWindowMetadataWithoutFalseAmbiguity(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	before := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	after := LocalPaneRef{GUIPID: 10, WindowID: 9, PaneID: 7}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(key, before); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(key, after); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(key); err != nil || got != after {
		t.Fatalf("Resolve moved pane = (%+v,%v), want (%+v,nil)", got, err, after)
	}
}

func TestMovedPaneStateFindsStableIdentityAndRefreshesWindow(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	before := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	after := LocalPaneRef{GUIPID: 10, WindowID: 9, PaneID: 7}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(key, before); err != nil {
		t.Fatal(err)
	}
	got, bound, live := r.SessionForPane(after)
	if !bound || !live || !got.Equal(key) {
		t.Fatalf("moved pane state = (%+v,%t,%t)", got, bound, live)
	}
	if route, err := r.Resolve(key); err != nil || route != after {
		t.Fatalf("refreshed route = (%+v,%v), want %+v", route, err, after)
	}
}

func TestMovedPaneUnbindUsesStableIdentity(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	before := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	after := LocalPaneRef{GUIPID: 10, WindowID: 9, PaneID: 7}
	if err := r.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(key, before); err != nil {
		t.Fatal(err)
	}
	r.UnbindPane(after)
	if _, bound, live := r.SessionForPane(before); bound || live {
		t.Fatalf("moved pane remained bound: bound=%t live=%t", bound, live)
	}
	if _, err := r.Resolve(key); !errors.Is(err, ErrSessionUnbound) {
		t.Fatalf("Resolve after moved unbind error = %v", err)
	}
}

func TestUnbindPaneSessionIgnoresMovedWindowButPreservesNewerBind(t *testing.T) {
	r := NewRegistry()
	oldRemote := exact("h", 1, "2026-08-24T20:00:00Z")
	newRemote := exact("h", 2, "2026-08-24T20:00:01Z")
	before := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	after := LocalPaneRef{GUIPID: 10, WindowID: 9, PaneID: 7}
	if err := r.Bind(oldRemote, before); err != nil {
		t.Fatal(err)
	}
	if !r.UnbindPaneSession(oldRemote, after) {
		t.Fatal("same old session was not removed after a window move")
	}
	if _, bound, _ := r.SessionForPane(before); bound {
		t.Fatal("old session remains bound")
	}

	// Model a local cleanup which paused after observing oldRemote while a
	// newer remote announcement claimed the same stable pane.
	if err := r.Bind(oldRemote, before); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(newRemote, after); err != nil {
		t.Fatal(err)
	}
	if r.UnbindPaneSession(oldRemote, before) {
		t.Fatal("stale local cleanup removed newer remote binding")
	}
	if got, bound, _ := r.SessionForPane(after); !bound || !got.Equal(newRemote) {
		t.Fatalf("newer binding = (%+v,%t), want (%+v,true)", got, bound, newRemote)
	}
}

func TestUnbindRouteRemovesOnlyExactCurrentRoute(t *testing.T) {
	r := NewRegistry()
	first := exact("h", 1, "2026-08-24T20:00:00Z")
	second := exact("h", 2, "2026-08-24T20:00:01Z")
	before := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	after := LocalPaneRef{GUIPID: 10, WindowID: 9, PaneID: 7}
	if err := r.ReplaceLive("h", []ExactSessionKey{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(first, before); err != nil {
		t.Fatal(err)
	}
	if r.UnbindRoute(second, before) {
		t.Fatal("different session removed current route")
	}
	if r.UnbindRoute(first, after) {
		t.Fatal("moved-window observation removed unmatched current ref")
	}
	if got, err := r.Resolve(first); err != nil || got != before {
		t.Fatalf("route changed after rejected prune: (%+v,%v)", got, err)
	}
	if !r.UnbindRoute(first, before) {
		t.Fatal("exact current route was not removed")
	}
	if _, err := r.Resolve(first); !errors.Is(err, ErrSessionUnbound) {
		t.Fatalf("Resolve after exact prune error = %v", err)
	}
}

func TestUnbindRouteDoesNotRemoveConcurrentRebind(t *testing.T) {
	r := NewRegistry()
	first := exact("h", 1, "2026-08-24T20:00:00Z")
	second := exact("h", 2, "2026-08-24T20:00:01Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	if err := r.ReplaceLive("h", []ExactSessionKey{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(first, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(second, pane); err != nil {
		t.Fatal(err)
	}
	if r.UnbindRoute(first, pane) {
		t.Fatal("stale focus attempt removed concurrent rebind")
	}
	if got, err := r.Resolve(second); err != nil || got != pane {
		t.Fatalf("rebound route = (%+v,%v), want (%+v,nil)", got, err, pane)
	}
}

func TestRefreshLiveRouteCannotOverwriteConcurrentPaneRebind(t *testing.T) {
	r := NewRegistry()
	first := exact("h", 1, "2026-08-24T20:00:00Z")
	second := exact("h", 2, "2026-08-24T20:00:01Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 7}
	if err := r.ReplaceLive("h", []ExactSessionKey{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(first, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(second, pane); err != nil {
		t.Fatal(err)
	}
	if r.RefreshLiveRoute(first, LocalPaneRef{GUIPID: 10, WindowID: 2, PaneID: 7}) {
		t.Fatal("stale session refreshed a pane rebound to another session")
	}
	if got, _, _ := r.SessionForPane(pane); !got.Equal(second) {
		t.Fatalf("pane session = %+v, want %+v", got, second)
	}
}

func TestReplaceLiveRejectsDuplicatePIDAtomically(t *testing.T) {
	r := NewRegistry()
	original := exact("h", 1, "2026-08-24T20:00:00Z")
	if err := r.ReplaceLive("h", []ExactSessionKey{original}); err != nil {
		t.Fatal(err)
	}
	err := r.ReplaceLive("h", []ExactSessionKey{
		exact("h", 2, "2026-08-24T20:00:00Z"),
		exact("h", 2, "2026-08-24T21:00:00Z"),
	})
	if !errors.Is(err, ErrDuplicateLivePID) {
		t.Fatalf("error = %v, want duplicate pid", err)
	}
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 1}
	if err := r.Bind(original, pane); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(original); err != nil || got != pane {
		t.Fatalf("original live set changed: (%+v,%v)", got, err)
	}
}

func TestRegistryCanonicalizesTimeZonesAndMonotonicValues(t *testing.T) {
	r := NewRegistry()
	localZone := exact("h", 1, "2026-08-24T13:00:00-07:00")
	utc := exact("h", 1, "2026-08-24T20:00:00Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 1}
	if err := r.Bind(localZone, pane); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceLive("h", []ExactSessionKey{utc}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(utc); err != nil || got != pane {
		t.Fatalf("Resolve = (%+v,%v)", got, err)
	}
}

func TestRegistryConcurrentBindingAndSnapshotReplacement(t *testing.T) {
	r := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	pane := LocalPaneRef{GUIPID: 10, WindowID: 1, PaneID: 1}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Bind(key, pane)
				_ = r.ReplaceLive("h", []ExactSessionKey{key})
				_, _ = r.Resolve(key)
			}
		}()
	}
	wg.Wait()
	if got, err := r.Resolve(key); err != nil || got != pane {
		t.Fatalf("final Resolve = (%+v,%v)", got, err)
	}
}
