//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/display"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/waybarchip"
	"golang.org/x/sys/unix"
)

func TestDisplayModeChangeWakesAtomicFileWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-mode")
	changes, closeWatch, err := watchDisplayFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWatch()
	mode, err := setDisplayMode(path, "toggle")
	if err != nil || mode != display.Circles {
		t.Fatalf("toggle: %s %v", mode, err)
	}
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("mode rename did not wake subscriber")
	}
	mode, err = setDisplayMode(path, "toggle")
	if err != nil || mode != display.Chips {
		t.Fatalf("second toggle: %s %v", mode, err)
	}
	if _, err = setDisplayMode(path, "invalid"); err == nil {
		t.Fatal("accepted invalid mode")
	}
	mode, err = display.ReadMode(path)
	if err != nil || mode != display.Chips {
		t.Fatalf("bad input changed mode: %s %v", mode, err)
	}
	closeWatch()
	// Drain any coalesced rename and observe shutdown without a leaked reader.
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("watch close did not cancel read")
		}
	}
}
func TestCirclesModeNeverLaunchesBottomBar(t *testing.T) {
	cfg := bottomLifecycleConfig(t)
	cfg.modeFile = filepath.Join(t.TempDir(), "mode")
	if _, err := setDisplayMode(cfg.modeFile, "circles"); err != nil {
		t.Fatal(err)
	}
	starts := 0
	cfg.ops.start = func(bottomBarConfig) error { starts++; return nil }
	reconcileWith(cfg, 20)
	if starts != 0 {
		t.Fatal("compact presentation launched bottom Waybar")
	}
	if _, err := setDisplayMode(cfg.modeFile, "chips"); err != nil {
		t.Fatal(err)
	}
	reconcileWith(cfg, 20)
	if starts != 1 {
		t.Fatalf("named presentation launches=%d", starts)
	}
}
func TestPresentationPublisherRetainsAllSessionsAcrossModeChange(t *testing.T) {
	dir := t.TempDir()
	cfg := bottomBarConfig{viewFile: filepath.Join(dir, "display.json"), circlesFile: filepath.Join(dir, "waybar.ini")}
	p := newPresentationPublisher(cfg)
	snap := state.Snapshot{}
	now := time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		snap.Sessions = append(snap.Sessions, state.Session{PID: i + 1, Remote: true, ResolvedName: "session <&>", CWD: "/src/project<&>", Hostname: "test", Navigable: true, StartedAt: now.Add(-time.Hour), LocalWorkspace: i + 1})
	}
	chips := waybarchip.NewRenderer(1000).RenderSlotsAt(snap, len(snap.Sessions), now)
	for _, mode := range []display.Mode{display.Chips, display.Circles} {
		if err := p.publish(snap, mode, true, true, now); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(cfg.viewFile)
		if err != nil {
			t.Fatal(err)
		}
		var frame display.Frame
		if err := json.Unmarshal(b, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Mode != mode || len(frame.Sessions) != 25 || frame.PublisherPID != os.Getpid() {
			t.Fatalf("bad shared frame: %+v", frame)
		}
		for i, session := range frame.Sessions {
			if session.TooltipMarkup != chips[i].Tooltip || !strings.Contains(session.TooltipMarkup, "&lt;&amp;&gt;") {
				t.Fatalf("session %d lost the shared escaped hover card: %s", i, session.TooltipMarkup)
			}
		}
	}
}

func TestQuietCircleTooltipBeyondBottomSlotsAdvancesWithoutSnapshot(t *testing.T) {
	cfg := bottomLifecycleConfig(t)
	cfg.modeFile = filepath.Join(cfg.slotDir, "mode")
	cfg.viewFile = filepath.Join(cfg.slotDir, "display.json")
	cfg.circlesFile = filepath.Join(cfg.slotDir, "circles.ini")
	if _, err := setDisplayMode(cfg.modeFile, "circles"); err != nil {
		t.Fatal(err)
	}
	cfg.presentation = newPresentationPublisher(cfg)
	snap := state.Snapshot{Sessions: make([]state.Session, 25)}
	for i := range snap.Sessions {
		snap.Sessions[i] = state.Session{PID: i + 1, Hostname: "test", Remote: true, ResolvedName: "session"}
	}
	// Only the last circle has a clock. Its minute boundary must wake the
	// renderer before the three-second recovery tick, with no fresh snapshot.
	since := time.Now().Add(-59 * time.Second)
	snap.Sessions[24].Claude = &state.AgentInfo{Status: state.StatusWorking, StatusSinceWire: &since}
	snapshots := make(chan state.Snapshot, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		renderDisplaySnapshots(cfg, waybarchip.NewRenderer(1000), newSlotPublisher(cfg.slotDir, bottomBarSlots), snapshots)
	}()
	defer func() { close(snapshots); <-done }()
	snapshots <- snap
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(cfg.viewFile)
		var frame display.Frame
		if json.Unmarshal(body, &frame) == nil && len(frame.Sessions) == 25 && strings.Contains(frame.Sessions[24].TooltipMarkup, "working · 1m") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("quiet circle hover did not advance at its minute boundary")
}

func TestLiveModeTransitionsKeepOneOwnerAndWakeOnExit(t *testing.T) {
	cfg := bottomLifecycleConfig(t)
	cfg.modeFile = filepath.Join(t.TempDir(), "mode")
	cfg.viewFile = filepath.Join(cfg.slotDir, "display.json")
	cfg.circlesFile = filepath.Join(cfg.slotDir, "circles.ini")
	changes, closeWatch, err := watchDisplayFiles(cfg.modeFile, cfg.marker)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWatch()
	cfg.controlEvents = changes
	cfg.lifecycleEvents = make(chan struct{}, 1)
	cfg.presentation = newPresentationPublisher(cfg)
	var starts atomic.Int32
	cfg.ops.start = func(c bottomBarConfig) error {
		b, err := os.ReadFile(cfg.viewFile)
		var frame display.Frame
		if err != nil || json.Unmarshal(b, &frame) != nil || frame.Mode != display.Chips {
			t.Error("bottom launched before circle surface was disabled")
		}
		starts.Add(1)
		return startBottom(c)
	}
	snapshots := make(chan state.Snapshot, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		renderDisplaySnapshots(cfg, waybarchip.NewRenderer(1000), newSlotPublisher(cfg.slotDir, bottomBarSlots), snapshots)
	}()
	defer func() {
		close(snapshots)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("display loop failed to exit")
		}
	}()
	snapshots <- state.Snapshot{Sessions: []state.Session{{PID: 42, Hostname: "test", Remote: true, Navigable: true, ResolvedName: "session", StartedAt: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)}}}
	waitFrame := func(mode display.Mode, visible bool) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			b, _ := os.ReadFile(cfg.viewFile)
			var frame display.Frame
			if json.Unmarshal(b, &frame) == nil && frame.Mode == mode && frame.Visible == visible && len(frame.Sessions) == 1 && frame.Sessions[0].PID == 42 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("no live frame for %s visible=%v", mode, visible)
	}
	waitFrame(display.Chips, true)
	waitBottomTest(t, "initial owned child", func() bool { _, err := os.Stat(cfg.pidFile); return err == nil })
	first := trackBottomTestChild(t, cfg)
	if _, err := setDisplayMode(cfg.modeFile, "circles"); err != nil {
		t.Fatal(err)
	}
	waitFrame(display.Circles, false)
	if starts.Load() != 1 {
		t.Fatal("mode toggle created an extra owner")
	}
	// The helper delays termination. Circles become visible on its exit event,
	// without another daemon snapshot or waiting for the three-second ticker.
	if err := unix.PidfdSendSignal(first.pidfd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	waitFrame(display.Circles, true)
	if _, err := setDisplayMode(cfg.modeFile, "chips"); err != nil {
		t.Fatal(err)
	}
	waitFrame(display.Chips, true)
	waitBottomTest(t, "replacement launch", func() bool { return starts.Load() == 2 })
	waitBottomTest(t, "replacement ownership record", func() bool { return bottomPID(cfg) > 0 })
	second := trackBottomTestChild(t, cfg)
	if first.pid == second.pid || starts.Load() != 2 {
		t.Fatal("incorrect surface replacement lifecycle")
	}
}

func TestDisplayPublisherHasOneLifetimeOwner(t *testing.T) {
	dir := t.TempDir()
	first, err := claimDisplayPublisher(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := claimDisplayPublisher(dir); err == nil {
		second.Close()
		t.Fatal("second display publisher acquired ownership")
	}
	first.Close()
	replacement, err := claimDisplayPublisher(dir)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()
}
