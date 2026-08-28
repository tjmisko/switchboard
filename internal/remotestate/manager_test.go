package remotestate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/federation"
	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/state"
)

type fakeProcess struct {
	stdout io.ReadCloser
	start  func() error
	wait   func() error
}

func (p *fakeProcess) StdoutPipe() (io.ReadCloser, error) { return p.stdout, nil }
func (p *fakeProcess) Start() error {
	if p.start != nil {
		return p.start()
	}
	return nil
}
func (p *fakeProcess) Wait() error {
	if p.wait != nil {
		return p.wait()
	}
	return nil
}

func TestNewSSHCommandUsesFixedArgumentVector(t *testing.T) {
	process, err := newSSHCommand(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}
	command := process.(execProcess).Cmd
	want := []string{
		"ssh", "-n", "-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ClearAllForwardings=yes",
		"build", "switchboard-ctl", "remote-stream",
	}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("ssh argv = %q, want %q", command.Args, want)
	}
	foundNoAskpass := false
	for _, variable := range command.Env {
		if variable == "SSH_ASKPASS_REQUIRE=never" {
			foundNoAskpass = true
		}
	}
	if !foundNoAskpass {
		t.Fatal("ssh child did not disable askpass")
	}
}

func TestEnvironmentWithOverridesExistingAskpassPolicy(t *testing.T) {
	got := environmentWith([]string{"A=1", "SSH_ASKPASS_REQUIRE=force", "B=2", "SSH_ASKPASS_REQUIRE=prefer"}, "SSH_ASKPASS_REQUIRE", "never")
	want := []string{"A=1", "B=2", "SSH_ASKPASS_REQUIRE=never"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environmentWith = %q, want %q", got, want)
	}
}

func TestNewManagerRejectsUnsafeAndDuplicateDestinations(t *testing.T) {
	for _, destinations := range [][]string{{"-oProxyCommand=bad"}, {"build box"}, {"build", "build"}} {
		if _, err := NewManager(ManagerConfig{Destinations: destinations}); err == nil {
			t.Fatalf("NewManager(%q) unexpectedly succeeded", destinations)
		}
	}
	if _, err := NewManager(ManagerConfig{Destinations: []string{"build"}, RetryDelay: MaxRetryDelay + time.Nanosecond}); err == nil {
		t.Fatal("NewManager accepted unbounded retry delay")
	}
	if _, err := NewManager(ManagerConfig{Destinations: []string{"build"}, MaxFrameBytes: DefaultMaxFrameBytes + 1}); err == nil {
		t.Fatal("NewManager accepted unbounded frame size")
	}
}

func TestManagerWorkerNeverOverlapsChildrenAndRemovesSnapshotBeforeRetry(t *testing.T) {
	frame := encodedFrame(t, "buildbox", testSnapshot(42))
	var mu sync.Mutex
	active := false
	attempts := 0
	waits := 0
	overlapped := false
	factory := func(context.Context, string) (Process, error) {
		mu.Lock()
		defer mu.Unlock()
		if active {
			overlapped = true
		}
		active = true
		attempts++
		return &fakeProcess{
			stdout: io.NopCloser(bytes.NewReader(frame)),
			wait: func() error {
				mu.Lock()
				active = false
				waits++
				mu.Unlock()
				return nil
			},
		}, nil
	}
	retries := 0
	manager, err := NewManager(ManagerConfig{
		Destinations: []string{"build"},
		Commands:     factory,
		WaitRetry: func(context.Context, time.Duration) bool {
			retries++
			return retries == 1
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	updates, cancel := manager.Subscribe()
	defer cancel()
	if err := manager.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotAttempts, gotWaits, gotOverlap := attempts, waits, overlapped
	mu.Unlock()
	if gotAttempts != 2 || gotWaits != 2 || gotOverlap {
		t.Fatalf("attempts=%d waits=%d overlapped=%v; want 2, 2, false", gotAttempts, gotWaits, gotOverlap)
	}
	if live := manager.Snapshot(); len(live) != 0 {
		t.Fatalf("snapshot retained after EOF: %+v", live)
	}
	for i, wantLive := range []bool{true, false, true, false} {
		select {
		case update := <-updates:
			_, live := update["buildbox"]
			if live != wantLive {
				t.Fatalf("update %d live=%v, want %v: %+v", i, live, wantLive, update)
			}
		default:
			t.Fatalf("missing update %d", i)
		}
	}
}

func TestManagerHostnameClaimIsStickyAcrossDisconnect(t *testing.T) {
	var removedHosts []string
	callbackSawVisibleHost := false
	var manager *Manager
	manager, err := NewManager(ManagerConfig{
		Destinations: []string{"primary", "alias"},
		OnHostRemoved: func(host string) {
			removedHosts = append(removedHosts, host)
			_, callbackSawVisibleHost = manager.Snapshot()[host]
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	updates, cancel := manager.Subscribe()
	defer cancel()
	frame := Frame{Host: "buildbox", Snapshot: testSnapshot(42)}
	if err := manager.accept("primary", frame); err != nil {
		t.Fatal(err)
	}
	<-updates // initial live replacement
	if _, removed := manager.removeLive("primary"); !removed {
		t.Fatal("primary snapshot was not removed")
	}
	if !reflect.DeepEqual(removedHosts, []string{"buildbox"}) {
		t.Fatalf("removed host callbacks = %v, want [buildbox]", removedHosts)
	}
	if callbackSawVisibleHost {
		t.Fatal("removing host remained visible during route/focus invalidation callback")
	}
	if disconnected := <-updates; len(disconnected) != 0 {
		t.Fatalf("disconnect replacement = %+v, want empty", disconnected)
	}
	if err := manager.accept("alias", frame); !errors.Is(err, ErrDuplicateHost) {
		t.Fatalf("alias claim error = %v, want ErrDuplicateHost", err)
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("duplicate alias restored rows: %+v", got)
	}
	if err := manager.accept("primary", frame); err != nil {
		t.Fatalf("original destination could not reclaim live rows: %v", err)
	}
}

func TestManagerTombstonePreventsAnotherSourceRepublishingRemovingHost(t *testing.T) {
	registry := panebind.NewRegistry()
	removalStarted := make(chan struct{})
	releaseRemoval := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRemoval) }) }
	defer release()

	manager, err := NewManager(ManagerConfig{
		Destinations: []string{"source-a", "source-b"},
		OnHostRemoved: func(host string) {
			registry.DropLiveHost(host)
			close(removalStarted)
			<-releaseRemoval
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancelRoutes := context.WithCancel(context.Background())
	defer cancelRoutes()
	routesChanged := make(chan struct{}, 8)
	routesReady := make(chan struct{})
	routesDone := make(chan error, 1)
	go func() {
		routesDone <- federation.RunLiveRoutesReady(ctx, manager, registry, nil, func() {
			routesChanged <- struct{}{}
		}, routesReady)
	}()
	<-routesReady
	<-routesChanged // initial empty replacement

	alphaSnapshot := testSnapshot(101)
	alphaKey := panebind.ExactSessionKey{
		Hostname:  "alpha",
		PID:       alphaSnapshot.Sessions[0].PID,
		StartedAt: alphaSnapshot.Sessions[0].StartedAt,
	}
	alphaPane := panebind.LocalPaneRef{GUIPID: 10, WindowID: 20, PaneID: 30}
	if err := registry.Bind(alphaKey, alphaPane); err != nil {
		t.Fatal(err)
	}
	if err := manager.accept("source-a", Frame{Host: "alpha", Snapshot: alphaSnapshot}); err != nil {
		t.Fatal(err)
	}
	<-routesChanged
	if _, err := registry.Resolve(alphaKey); err != nil {
		t.Fatalf("alpha route was not initially live: %v", err)
	}

	removeDone := make(chan struct{})
	go func() {
		manager.removeLive("source-a")
		close(removeDone)
	}()
	<-removalStarted
	if _, visible := manager.Snapshot()["alpha"]; visible {
		t.Fatal("alpha remained visible while its removal callback was blocked")
	}

	// Model a fresh terminal announcement arriving after the callback dropped
	// alpha's old candidates. A concurrent source publication must not carry
	// alpha back into the complete live map and authorize this new candidate.
	if err := registry.Bind(alphaKey, alphaPane); err != nil {
		t.Fatal(err)
	}
	if err := manager.accept("source-b", Frame{Host: "beta", Snapshot: testSnapshot(202)}); err != nil {
		t.Fatal(err)
	}
	<-routesChanged
	if _, bound, live := registry.SessionForPane(alphaPane); bound || live {
		t.Fatalf("concurrent source publication restored removing alpha route: bound=%v live=%v", bound, live)
	}
	if _, err := registry.Resolve(alphaKey); !errors.Is(err, panebind.ErrSessionNotLive) {
		t.Fatalf("Resolve(alpha) error = %v, want ErrSessionNotLive", err)
	}

	release()
	<-removeDone
	cancelRoutes()
	if err := <-routesDone; err != nil {
		t.Fatal(err)
	}
}

func TestManagerValidatesFramesEvenWithInjectedReader(t *testing.T) {
	manager, err := NewManager(ManagerConfig{Destinations: []string{"build"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.accept("build", Frame{Host: "BuildBox", Snapshot: testSnapshot(42)}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("noncanonical frame error = %v, want ErrInvalidFrame", err)
	}
	old := testSnapshot(42)
	old.SchemaVersion--
	if err := manager.accept("build", Frame{Host: "buildbox", Snapshot: old}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("old-schema frame error = %v, want ErrSchemaMismatch", err)
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("invalid injected frames entered live map: %+v", got)
	}
}

func TestManagerRejectsLocalHostnameBeforePublishing(t *testing.T) {
	var diagnostics []Diagnostic
	manager, err := NewManager(ManagerConfig{
		Destinations:  []string{"localhost-alias"},
		LocalHostname: "ClientBox.",
		Commands: func(context.Context, string) (Process, error) {
			return &fakeProcess{stdout: io.NopCloser(bytes.NewReader(encodedFrame(t, "clientbox", testSnapshot(42))))}, nil
		},
		WaitRetry:    func(context.Context, time.Duration) bool { return false },
		OnDiagnostic: func(diagnostic Diagnostic) { diagnostics = append(diagnostics, diagnostic) },
	})
	if err != nil {
		t.Fatal(err)
	}
	updates, cancel := manager.Subscribe()
	defer cancel()
	if err := manager.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("local-host collision was published: %+v", got)
	}
	select {
	case update := <-updates:
		t.Fatalf("local-host collision notified subscribers: %+v", update)
	default:
	}
	if len(diagnostics) != 1 || diagnostics[0].Category != DiagnosticLocalHost || diagnostics[0].Host != "clientbox" {
		t.Fatalf("diagnostics = %+v, want local_hostname/clientbox", diagnostics)
	}
}

func TestManagerRejectsHostnameChangeAndRemovesPriorRows(t *testing.T) {
	input := append(encodedFrame(t, "one", testSnapshot(1)), encodedFrame(t, "two", testSnapshot(2))...)
	var diagnostics []Diagnostic
	manager, err := NewManager(ManagerConfig{
		Destinations: []string{"build"},
		Commands: func(context.Context, string) (Process, error) {
			return &fakeProcess{stdout: io.NopCloser(bytes.NewReader(input))}, nil
		},
		WaitRetry:    func(context.Context, time.Duration) bool { return false },
		OnDiagnostic: func(diagnostic Diagnostic) { diagnostics = append(diagnostics, diagnostic) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("rows survived hostname flip: %+v", got)
	}
	if len(diagnostics) != 1 || diagnostics[0].Category != DiagnosticHostnameFlip || diagnostics[0].Host != "one" {
		t.Fatalf("diagnostics = %+v, want one hostname_changed for one", diagnostics)
	}
}

func TestManagerReaderSeamRejectsSourceAndWaitsForChild(t *testing.T) {
	waited := false
	var diagnostics []Diagnostic
	manager, err := NewManager(ManagerConfig{
		Destinations: []string{"build"},
		Commands: func(context.Context, string) (Process, error) {
			return &fakeProcess{
				stdout: io.NopCloser(bytes.NewReader(nil)),
				wait:   func() error { waited = true; return nil },
			}, nil
		},
		ReadFrames: func(_ io.Reader, _ int, accept func(Frame) error) error {
			if err := accept(Frame{Host: "buildbox", Snapshot: testSnapshot(42)}); err != nil {
				return err
			}
			return ErrSchemaMismatch
		},
		WaitRetry:    func(context.Context, time.Duration) bool { return false },
		OnDiagnostic: func(diagnostic Diagnostic) { diagnostics = append(diagnostics, diagnostic) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !waited {
		t.Fatal("manager did not wait for rejected child")
	}
	if got := manager.Snapshot(); len(got) != 0 {
		t.Fatalf("rejected source retained rows: %+v", got)
	}
	if len(diagnostics) != 1 || diagnostics[0].Category != DiagnosticSchema {
		t.Fatalf("diagnostics = %+v, want schema mismatch", diagnostics)
	}
}

func TestManagerCancellationClosesReaderAndWaits(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	started := make(chan struct{})
	waited := make(chan struct{})
	manager, err := NewManager(ManagerConfig{
		Destinations: []string{"build"},
		Commands: func(ctx context.Context, _ string) (Process, error) {
			return &fakeProcess{
				stdout: reader,
				start:  func() error { close(started); return nil },
				wait: func() error {
					<-ctx.Done()
					close(waited)
					return ctx.Err()
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	select {
	case <-waited:
	default:
		t.Fatal("Run returned before child Wait")
	}
}

func TestManagerPublishesDisconnectBeforeSlowWaitButDoesNotRetry(t *testing.T) {
	frame := encodedFrame(t, "buildbox", testSnapshot(42))
	waitEntered := make(chan struct{})
	releaseWait := make(chan struct{})
	manager, err := NewManager(ManagerConfig{
		Destinations: []string{"build"},
		Commands: func(context.Context, string) (Process, error) {
			return &fakeProcess{
				stdout: io.NopCloser(bytes.NewReader(frame)),
				wait: func() error {
					close(waitEntered)
					<-releaseWait
					return nil
				},
			}, nil
		},
		WaitRetry: func(context.Context, time.Duration) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	updates, cancel := manager.Subscribe()
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.Run(context.Background()) }()
	if live := <-updates; len(live) != 1 {
		t.Fatalf("first replacement = %+v, want live host", live)
	}
	if disconnected := <-updates; len(disconnected) != 0 {
		t.Fatalf("disconnect replacement = %+v, want empty", disconnected)
	}
	<-waitEntered
	select {
	case err := <-done:
		t.Fatalf("manager returned before child Wait was released: %v", err)
	default:
	}
	close(releaseWait)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestManagerSnapshotsAndSubscriptionsAreDetachedFromInternalState(t *testing.T) {
	manager, err := NewManager(ManagerConfig{Destinations: []string{"build"}})
	if err != nil {
		t.Fatal(err)
	}
	updates, cancel := manager.Subscribe()
	defer cancel()
	if err := manager.accept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(42)}); err != nil {
		t.Fatal(err)
	}
	update := <-updates
	update["buildbox"].Sessions[0].CWD = "/mutated-subscription"
	view := manager.Snapshot()
	view["buildbox"].Sessions[0].CWD = "/mutated-snapshot"
	again := manager.Snapshot()
	if got := again["buildbox"].Sessions[0].CWD; got != "/work" {
		t.Fatalf("caller mutation reached manager: cwd=%q", got)
	}
}

func TestManagerSaturatedSubscriberStillReceivesNewestDisconnect(t *testing.T) {
	manager, err := NewManager(ManagerConfig{Destinations: []string{"build"}})
	if err != nil {
		t.Fatal(err)
	}
	updates, cancel := manager.Subscribe()
	defer cancel()
	for pid := 1; pid <= 12; pid++ {
		if err := manager.accept("build", Frame{Host: "buildbox", Snapshot: testSnapshot(pid)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, removed := manager.removeLive("build"); !removed {
		t.Fatal("expected final disconnect to remove live host")
	}
	var last map[string]state.Snapshot
	for {
		select {
		case last = <-updates:
			continue
		default:
			if last == nil {
				t.Fatal("subscriber received no updates")
			}
			if len(last) != 0 {
				t.Fatalf("last coalesced update is stale: %+v", last)
			}
			return
		}
	}
}

func TestManagerIsSingleUse(t *testing.T) {
	manager, err := NewManager(ManagerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Run(context.Background()); !errors.Is(err, ErrManagerAlreadyRun) {
		t.Fatalf("second Run error = %v, want ErrManagerAlreadyRun", err)
	}
}
