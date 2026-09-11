//go:build linux

package main

// The bottombar subcommand publishes the bottom ("claude") Waybar modules and
// enforces a single visibility invariant:
//
//	bottom bar runs  <=>  (top bar visible)  AND  (>=1 aggregate agent session)
//
// Legacy `watch` mode owns a dedicated `waybar -c claude.jsonc` process.
// `publish` mode attaches to a combined top+bottom Waybar process owned by the
// desktop session and toggles only its bottom window. Keeping both modes makes
// the configuration migration reversible instead of changing ownership and
// process topology in one unguarded step.
//
// Two inputs drive the invariant, each owned by a different actor:
//
//	top visible : the F8 master toggle, recorded as the presence/absence of a
//	              marker file (absent => visible). Owned by hypr-float-center.
//	sessions    : the switchboard daemon's aggregate session count.
//
// `bottombar watch` reacts to session changes (one subscribe stream + safety
// ticker), renders every chip into signal-triggered Waybar files, and owns the
// bar lifecycle. `bottombar reconcile` is the one-shot the F8 script calls
// after it flips the master toggle. Both lifecycle paths funnel through
// reconcile under a flock, so they never race each other.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/waybarchip"
	"golang.org/x/sys/unix"
)

const (
	bottomBarSlots       = 10
	waybarReadyPoll      = 25 * time.Millisecond
	waybarReadyPollMax   = 400 * time.Millisecond
	waybarReadyPollTries = 6
	waybarReadyRetry     = 3 * time.Second
	linuxGlibcSIGRTMIN   = 34
)

type bottomBarConfig struct {
	socketPath   string
	marker       string // master-visibility marker; present => top bar hidden
	pidFile      string
	lockFile     string
	readyFile    string
	visibleFile  string
	slotDir      string
	waybarConfig string
	attached     bool

	ops bottomBarOps
}

// bottomBarOps are the effectful lifecycle operations, injectable so the
// reconcile/start/stop orchestration is testable without launching a real
// waybar. defaultOps wires the production behavior; tests substitute a stub.
type bottomBarOps struct {
	isRunning func(bottomBarConfig) bool // is the bottom bar currently up?
	start     func(bottomBarConfig) error
	stop      func(bottomBarConfig)
}

// slotPublisher is a latest-value mailbox for each Waybar module. Regular
// files make the last frame sticky across Waybar and watcher restarts; a
// realtime signal only tells an already-running module to re-read its file.
// One broker goroutine owns this value, so it needs no mutex.
type slotPublisher struct {
	dir     string
	last    [][]byte
	dirty   []bool
	blocked bool
	replace func(string, []byte) error
	signal  func(int, int) error
}

func newSlotPublisher(dir string, slots int) *slotPublisher {
	return &slotPublisher{
		dir:     dir,
		last:    make([][]byte, slots),
		dirty:   make([]bool, slots),
		replace: replaceFile,
		signal: func(pidfd, slot int) error {
			// bottombar is a Linux/Waybar extra already (/proc, Setsid,
			// Hyprland). Waybar's signal=N contract maps to SIGRTMIN+N;
			// glibc reserves 32 and 33, so its userspace SIGRTMIN is 34.
			return unix.PidfdSendSignal(pidfd, unix.Signal(linuxGlibcSIGRTMIN+slot+1), nil, 0)
		},
	}
}

func (p *slotPublisher) path(slot int) string {
	return filepath.Join(p.dir, fmt.Sprintf("slot-%d.json", slot))
}

// stage atomically replaces only byte-changed slot documents. A successful
// rename happens before dirty is set, so flush can never signal a reader toward
// a partial or previous file.
func (p *slotPublisher) stage(outputs []waybarchip.Output) error {
	bodies := make([][]byte, len(p.last))
	changed := make([]bool, len(p.last))
	force := p.blocked
	for slot := range p.last {
		if slot >= len(outputs) {
			break
		}
		body, err := json.Marshal(outputs[slot])
		if err != nil {
			return fmt.Errorf("marshal slot %d: %w", slot, err)
		}
		body = append(body, '\n')
		bodies[slot] = body
		// After any partial replacement, last no longer describes every file on
		// disk. Rewrite the complete row before clearing the publication block.
		changed[slot] = force || !bytes.Equal(body, p.last[slot])
	}
	for slot := range p.last {
		if !changed[slot] {
			continue
		}
		if err := p.replace(p.path(slot), bodies[slot]); err != nil {
			// A prefix may already be on disk, but no signal may expose this
			// generation. Keep last unchanged so the next stage retries the full
			// replacement set before flushing anything.
			p.blocked = true
			return fmt.Errorf("publish slot %d: %w", slot, err)
		}
	}
	for slot := range p.last {
		if !changed[slot] {
			continue
		}
		p.last[slot] = bytes.Clone(bodies[slot])
		p.dirty[slot] = true
	}
	p.blocked = false
	return nil
}

func (p *slotPublisher) hasDirty() bool {
	for _, dirty := range p.dirty {
		if dirty {
			return true
		}
	}
	return false
}

// flush signals exactly the dirty modules. Failed signals stay dirty so the
// readiness poll or next snapshot retries them; a startup edge cannot disappear
// merely because Waybar had not installed its handlers yet.
func (p *slotPublisher) flush(pidfd int) {
	if pidfd < 0 || p.blocked {
		return
	}
	for slot, dirty := range p.dirty {
		if !dirty {
			continue
		}
		if err := p.signal(pidfd, slot); err == nil {
			p.dirty[slot] = false
		}
	}
}

// replaceFile writes beside the destination and renames over it. Readers see
// one complete JSON line or the previous complete line, never a truncation.
func replaceFile(path string, body []byte) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".slot-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(body)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func defaultOps() bottomBarOps {
	return bottomBarOps{
		isRunning: bottomMayBeRunning,
		start:     startBottom,
		stop:      stopBottom,
	}
}

func bottomBarConfigDefault(socketPath string) bottomBarConfig {
	run := runtimeDir()
	home, _ := os.UserHomeDir()
	return bottomBarConfig{
		socketPath:   socketPath,
		marker:       envOr("SWITCHBOARD_WAYBAR_MARKER", "/tmp/hypr-float-center/waybar-hidden"),
		pidFile:      filepath.Join(run, "switchboard", "bottom-waybar.pid"),
		lockFile:     filepath.Join(run, "switchboard", "bottombar.lock"),
		readyFile:    filepath.Join(run, "switchboard", "bottom-waybar.ready"),
		visibleFile:  filepath.Join(run, "switchboard", "bottom-waybar.visible"),
		slotDir:      filepath.Join(run, "switchboard"),
		waybarConfig: envOr("SWITCHBOARD_BOTTOM_CONFIG", filepath.Join(home, ".config", "waybar", "claude.jsonc")),
		ops:          defaultOps(),
	}
}

// cmdBottombar dispatches the bottombar subcommands. It deliberately runs
// before the daemon dial in main(), because `watch` must tolerate the daemon
// being down and reconnect on its own.
func cmdBottombar(args []string, socketPath string) {
	sub := "reconcile"
	if len(args) > 0 {
		sub = args[0]
	}
	cfg := bottomBarConfigDefault(socketPath)
	if err := os.MkdirAll(filepath.Dir(cfg.pidFile), 0o755); err != nil {
		fail("bottombar: %v", err)
	}

	switch sub {
	case "reconcile":
		reconcile(cfg)
	case "watch":
		watchBottomBar(cfg)
	case "publish":
		cfg.attached = true
		watchBottomBar(cfg)
	case "reconcile-attached":
		cfg.attached = true
		reconcile(cfg)
	case "stop":
		unlock := mustFlock(cfg.lockFile)
		ensureStopped(cfg)
		unlock()
	default:
		fail("bottombar: unknown subcommand %q (want publish|reconcile-attached|watch|reconcile|stop)", sub)
	}
}

// shouldRun is the bottom-bar invariant, distilled to a pure decision: the
// bottom bar runs iff the top bar is visible AND at least one agent session
// exists. Both reconcile paths route their final start/stop decision through
// it, so the F8 truth table has exactly one source of truth.
func shouldRun(topVisible bool, count int) bool {
	return topVisible && count > 0
}

// reconcile brings the bottom bar in line with the invariant, dialing the
// daemon for the current session count. Safe to call concurrently — it holds
// the flock for the duration.
func reconcile(cfg bottomBarConfig) {
	unlock := mustFlock(cfg.lockFile)
	if !topVisible(cfg) {
		// Master toggle is off: the bottom bar must not exist regardless of
		// session count. We can decide this without the daemon.
		ensureStopped(cfg)
		unlock()
		return
	}
	unlock()

	// Never hold the lifecycle flock across a daemon round-trip. The renderer
	// uses the same flock to fence PID validation and RT signaling against an F8
	// stop/start, so socket latency must not enter that critical path.
	count, ok := sessionCount(cfg.socketPath)
	if !ok {
		// Daemon unreachable — we cannot know the session count, so leave the
		// bottom bar in whatever state it is. Better than flapping.
		return
	}

	unlock = mustFlock(cfg.lockFile)
	defer unlock()
	// Visibility may have changed while the daemon replied; re-read it under
	// the lifecycle lock before deciding process state.
	setBottom(cfg, shouldRun(topVisible(cfg), count))
}

// reconcileWith is reconcile when the caller already knows the session count
// (e.g. from a subscribe snapshot), avoiding a redundant daemon round-trip.
func reconcileWith(cfg bottomBarConfig, count int) {
	unlock := mustFlock(cfg.lockFile)
	defer unlock()
	setBottom(cfg, shouldRun(topVisible(cfg), count))
}

func setBottom(cfg bottomBarConfig, run bool) {
	if cfg.attached {
		setAttachedBottom(cfg, run)
		return
	}
	if run {
		ensureStarted(cfg)
	} else {
		ensureStopped(cfg)
	}
}

// watchBottomBar runs forever, reconciling the bottom bar on every session
// change. The subscribe stream gives instant reaction; the ticker is a safety
// net for dropped snapshots (the daemon's subscriber channel drops on lag) and
// for any master-toggle path that does not call reconcile directly.
func watchBottomBar(cfg bottomBarConfig) {
	renderer := waybarchip.NewRenderer(0)
	publisher := newSlotPublisher(cfg.slotDir, bottomBarSlots)
	// Materialize every module's startup document before any watcher-owned
	// reconcile can launch Waybar. Empty slots are valid Waybar JSON, so startup
	// never depends on a pipe writer arriving in time.
	if err := publisher.stage(renderer.RenderSlotsAt(state.Snapshot{}, bottomBarSlots, time.Now())); err != nil {
		fail("bottombar: preseed: %v", err)
	}

	for {
		streamSnapshots(cfg, renderer, publisher)
		// Match the old slot processes' disconnect behavior: blank their widgets
		// but leave the Waybar lifecycle unchanged while the daemon restarts.
		if err := publisher.stage(renderer.RenderSlotsAt(state.Snapshot{}, bottomBarSlots, time.Now())); err != nil {
			fmt.Fprintf(os.Stderr, "bottombar: disconnect publish: %v\n", err)
		}
		flushSlotsWhenReady(cfg, publisher)
		// Connection dropped. Reconcile once (the daemon may be restarting),
		// then retry. The ticker keeps things honest in the meantime.
		reconcile(cfg)
		time.Sleep(2 * time.Second)
	}
}

// streamSnapshots subscribes and reconciles on each snapshot until the
// connection drops, then returns.
func streamSnapshots(cfg bottomBarConfig, renderer *waybarchip.Renderer, publisher *slotPublisher) {
	c, err := rpc.Dial(cfg.socketPath)
	if err != nil {
		return
	}
	defer c.Close()
	command := "subscribe-all"
	if err := c.Send(rpc.Request{Cmd: command}); err != nil {
		return
	}
	snapshots := make(chan state.Snapshot, 1)
	go receiveBottomSnapshots(c, command, snapshots)

	var latest state.Snapshot
	var haveLatest bool
	var readyPID, readyTries int
	var readyWarned bool
	var readyRetryAt time.Time
	var refreshTimer, readyTimer *time.Timer
	var refreshC, readyC <-chan time.Time
	safety := time.NewTicker(3 * time.Second)
	defer func() {
		safety.Stop()
		if refreshTimer != nil {
			refreshTimer.Stop()
		}
		if readyTimer != nil {
			readyTimer.Stop()
		}
	}()

	reset := func(timer **time.Timer, ch *<-chan time.Time, delay time.Duration) {
		if delay < 0 {
			delay = 0
		}
		if *timer == nil {
			*timer = time.NewTimer(delay)
		} else {
			if !(*timer).Stop() {
				select {
				case <-(*timer).C:
				default:
				}
			}
			(*timer).Reset(delay)
		}
		*ch = (*timer).C
	}
	stop := func(timer *time.Timer, ch *<-chan time.Time) {
		if timer != nil {
			timer.Stop()
		}
		*ch = nil
	}
	scheduleReady := func() {
		if !publisher.hasDirty() {
			stop(readyTimer, &readyC)
			readyPID, readyTries, readyWarned = 0, 0, false
			readyRetryAt = time.Time{}
			return
		}
		if readyWarned && time.Now().Before(readyRetryAt) {
			return
		}
		flushed, pid := flushSlotsWhenReady(cfg, publisher)
		if pid != readyPID {
			stop(readyTimer, &readyC)
			readyPID, readyTries, readyWarned = pid, 0, false
			readyRetryAt = time.Time{}
		}
		if flushed {
			stop(readyTimer, &readyC)
			readyPID, readyTries, readyWarned = 0, 0, false
			readyRetryAt = time.Time{}
			return
		}
		if pid <= 0 || readyC != nil {
			return
		}
		delay, retry := nextWaybarReadyDelay(readyTries)
		if !retry {
			if !readyWarned {
				fmt.Fprintln(os.Stderr, "bottombar: Waybar never acknowledged signal-mode readiness; check the active Waybar config")
				readyWarned = true
			}
			readyRetryAt = time.Now().Add(waybarReadyRetry)
			return
		}
		readyTries++
		reset(&readyTimer, &readyC, delay)
	}
	render := func(now time.Time) bool {
		if err := publisher.stage(renderer.RenderSlotsAt(latest, bottomBarSlots, now)); err != nil {
			fmt.Fprintf(os.Stderr, "bottombar: render: %v\n", err)
			return false
		}
		reconcileWith(cfg, len(latest.Sessions))
		scheduleReady()
		next := renderer.NextRefresh(latest, bottomBarSlots, now)
		if next.IsZero() {
			stop(refreshTimer, &refreshC)
		} else {
			reset(&refreshTimer, &refreshC, next.Sub(now))
		}
		return true
	}

	for {
		select {
		case snapshot, ok := <-snapshots:
			if !ok {
				return
			}
			latest, haveLatest = snapshot, true
			render(time.Now())
		case <-refreshC:
			render(time.Now())
		case <-readyC:
			readyC = nil
			scheduleReady()
		case <-safety.C:
			if haveLatest {
				render(time.Now())
			} else {
				reconcile(cfg)
				scheduleReady()
			}
		}
	}
}

type bottomSnapshotStream interface {
	Send(rpc.Request) error
	Recv(*rpc.Response) error
}

func nextWaybarReadyDelay(attempt int) (time.Duration, bool) {
	if attempt < 0 || attempt >= waybarReadyPollTries {
		return 0, false
	}
	delay := waybarReadyPoll << attempt
	if delay > waybarReadyPollMax {
		delay = waybarReadyPollMax
	}
	return delay, true
}

func receiveBottomSnapshots(c bottomSnapshotStream, command string, snapshots chan state.Snapshot) {
	// Closing a buffered channel preserves its final queued value: streamSnapshots
	// must consume that replacement before it observes ok=false and disconnects.
	// A separate error channel would race the final snapshot in select.
	defer close(snapshots)
	for {
		var resp rpc.Response
		if err := c.Recv(&resp); err != nil {
			return
		}
		if resp.Error != "" {
			if command == "subscribe-all" && unsupportedRPCCommand(resp.Error, command) {
				fmt.Fprintln(os.Stderr, "bottombar: daemon lacks aggregate subscriptions; using local sessions")
				command = "subscribe"
				if err := c.Send(rpc.Request{Cmd: command}); err != nil {
					return
				}
				continue
			}
			fmt.Fprintf(os.Stderr, "bottombar: %s: %s\n", command, resp.Error)
			return
		}
		if resp.Snapshot == nil {
			continue
		}
		// Full replacements make latest-wins coalescing safe. Do not let a slow
		// render loop backpressure the daemon's socket writer.
		select {
		case snapshots <- *resp.Snapshot:
		default:
			select {
			case <-snapshots:
			default:
			}
			snapshots <- *resp.Snapshot
		}
	}
}

// flushSlotsWhenReady suppresses realtime signals until module zero has run
// once in the newly started Waybar process and written its parent's PID to the
// ready file. Dirty bits survive the wait, so the poll always catches up to the
// newest atomically published files rather than replaying an old frame.
// The lifecycle flock makes the pidfile/starttime check and signal one atomic
// decision with respect to an F8-triggered stop/start.
func flushSlotsWhenReady(cfg bottomBarConfig, publisher *slotPublisher) (flushed bool, pid int) {
	unlock := mustFlock(cfg.lockFile)
	defer unlock()
	process, ok := openConfiguredProcess(cfg)
	if !ok {
		return false, 0
	}
	defer unix.Close(process.pidfd)
	return flushSlotsForProcess(cfg, publisher, process, process.pidfd), process.pid
}

func flushSlotsForProcess(cfg bottomBarConfig, publisher *slotPublisher, process bottomProcess, pidfd int) bool {
	if process.pid == 0 || !bottomBarReady(cfg, process) {
		return false
	}
	publisher.flush(pidfd)
	return !publisher.hasDirty()
}

func bottomBarReady(cfg bottomBarConfig, process bottomProcess) bool {
	b, err := os.ReadFile(cfg.readyFile)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 || len(fields) > 2 {
		return false
	}
	readyPID, err := strconv.Atoi(fields[0])
	if err != nil || readyPID != process.pid {
		return false
	}
	if len(fields) == 1 {
		return !cfg.attached
	}
	readyStarted, err := strconv.ParseUint(fields[1], 10, 64)
	return err == nil && readyStarted == process.started
}

// topVisible reports whether the top bar's master toggle is on. The toggle is
// the absence of the marker file (hypr-float-center touches it to hide).
func topVisible(cfg bottomBarConfig) bool {
	_, err := os.Stat(cfg.marker)
	return os.IsNotExist(err)
}

// sessionCount asks the daemon how many sessions exist. The bool is false if
// the daemon could not be reached.
func sessionCount(socketPath string) (int, bool) {
	c, err := rpc.Dial(socketPath)
	if err != nil {
		return 0, false
	}
	defer c.Close()
	command := "list-all"
	for {
		if err := c.Send(rpc.Request{Cmd: command}); err != nil {
			return 0, false
		}
		var resp rpc.Response
		if err := c.Recv(&resp); err != nil {
			return 0, false
		}
		if resp.Error != "" {
			if command == "list-all" && unsupportedRPCCommand(resp.Error, command) {
				command = "list"
				continue
			}
			fmt.Fprintf(os.Stderr, "bottombar: %s: %s\n", command, resp.Error)
			return 0, false
		}
		if resp.Snapshot == nil {
			return 0, true
		}
		return len(resp.Snapshot.Sessions), true
	}
}

func unsupportedRPCCommand(message, command string) bool {
	return strings.TrimSpace(message) == "unknown cmd: "+command
}

// ensureStarted launches the bottom waybar if it is not already running.
func ensureStarted(cfg bottomBarConfig) {
	if cfg.ops.isRunning(cfg) {
		return
	}
	if err := cfg.ops.start(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "bottombar: start: %v\n", err)
	}
}

// ensureStopped requests shutdown of the bottom Waybar. Ownership remains
// recorded until that process generation exits.
func ensureStopped(cfg bottomBarConfig) {
	cfg.ops.stop(cfg)
}

// stopBottom requests termination through the exact process's pidfd. Keep the
// identity until exit is observed: SIGTERM can be delayed or ignored, and a
// subsequent show must not launch a second bar while this one is still alive.
func stopBottom(cfg bottomBarConfig) {
	if process, ok := openBottomProcess(cfg); ok {
		defer unix.Close(process.pidfd)
		if err := unix.PidfdSendSignal(process.pidfd, unix.SIGTERM, nil, 0); err != nil && err != unix.ESRCH {
			fmt.Fprintf(os.Stderr, "bottombar: terminate pid=%d: %v\n", process.pid, err)
		}
	}
	_ = os.Remove(cfg.readyFile)
}

// startBottom spawns `waybar -c <claude config>` detached into its own session
// so it survives this process, and records its pid. Waybar diagnostics remain
// connected to the watcher's stdout/stderr so systemd captures launch failures.
func startBottom(cfg bottomBarConfig) error {
	// A ready file belongs to one exact PID. Clear the previous generation
	// before launch so the broker cannot send an RT signal while the replacement
	// process still has the signal's default (terminating) disposition.
	_ = os.Remove(cfg.readyFile)
	cmd := exec.Command("waybar", "-c", cfg.waybarConfig)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if dn, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		configureBottomCommandIO(cmd, dn)
		defer dn.Close()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	reapFailedStart := func() {
		// This is still our unreaped child, so its process-group identity cannot
		// be recycled before Wait. Clean up any startup module shells too.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	started, err := processStartTime(pid)
	if err != nil {
		reapFailedStart()
		return fmt.Errorf("read waybar start time: %w", err)
	}
	// Publish the identity atomically. It keeps diagnostic/legacy readers safe
	// too, and prevents a partial record if the watcher is replaced mid-write.
	if err := replaceFile(cfg.pidFile, []byte(fmt.Sprintf("%d %d\n", pid, started))); err != nil {
		reapFailedStart()
		return err
	}
	fmt.Fprintf(os.Stderr, "bottombar: launched pid=%d starttime=%d\n", pid, started)
	// Wait only for our own child. A global Wait4(-1) reaper could consume a
	// failed launch before its cleanup, invalidating process-group ownership.
	// One-shot callers may exit first; the child is then reparented normally.
	go func() {
		err := cmd.Wait()
		fmt.Fprintf(os.Stderr, "bottombar: exited pid=%d starttime=%d result=%v\n", pid, started, err)
	}()
	return nil
}

func configureBottomCommandIO(cmd *exec.Cmd, devNull *os.File) {
	cmd.Stdin = devNull
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
}

type bottomProcess struct {
	pid     int
	pidfd   int
	started uint64
}

func openConfiguredProcess(cfg bottomBarConfig) (bottomProcess, bool) {
	if cfg.attached {
		return openAttachedProcess(cfg)
	}
	return openBottomProcess(cfg)
}

// openAttachedProcess binds the process that loaded the combined Waybar
// configuration. The module-created ready record is the attachment handshake;
// unlike legacy mode, the publisher never infers ownership from a command line
// and never starts or kills this process.
func openAttachedProcess(cfg bottomBarConfig) (bottomProcess, bool) {
	b, err := os.ReadFile(cfg.readyFile)
	if err != nil {
		return bottomProcess{}, false
	}
	fields := strings.Fields(string(b))
	// Combined mode requires the process start time as a generation fence. A
	// PID-only record belongs to the legacy split configuration and must not be
	// adopted during a partial rollout.
	if len(fields) != 2 {
		return bottomProcess{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return bottomProcess{}, false
	}
	recordedStart, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return bottomProcess{}, false
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return bottomProcess{}, false
	}
	fail := func() (bottomProcess, bool) {
		_ = unix.Close(pidfd)
		return bottomProcess{}, false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil || strings.TrimSpace(string(comm)) != "waybar" {
		return fail()
	}
	started, err := processStartTime(pid)
	if err != nil || started != recordedStart {
		return fail()
	}
	return bottomProcess{pid: pid, pidfd: pidfd, started: started}, true
}

type attachedVisibility struct {
	pid     int
	started uint64
	visible bool
}

func readAttachedVisibility(path string) (attachedVisibility, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return attachedVisibility{}, false
	}
	fields := strings.Fields(string(b))
	if len(fields) != 3 {
		return attachedVisibility{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return attachedVisibility{}, false
	}
	started, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return attachedVisibility{}, false
	}
	visible, err := strconv.ParseBool(fields[2])
	if err != nil {
		return attachedVisibility{}, false
	}
	return attachedVisibility{pid: pid, started: started, visible: visible}, true
}

// setAttachedBottom reconciles the bottom window of a combined Waybar process.
// The shipped combined config starts that window hidden and maps SIGUSR2 to a
// bottom-only toggle. Persisting the acknowledged generation and state makes a
// one-shot F8 reconcile and the long-running publisher share one idempotence
// fence; a new Waybar generation always resets to its configured hidden state.
func setAttachedBottom(cfg bottomBarConfig, visible bool) {
	process, ok := openAttachedProcess(cfg)
	if !ok {
		return
	}
	defer unix.Close(process.pidfd)
	reconcileAttachedVisibility(cfg, process, visible, func() error {
		return unix.PidfdSendSignal(process.pidfd, unix.SIGUSR2, nil, 0)
	})
}

func reconcileAttachedVisibility(cfg bottomBarConfig, process bottomProcess, visible bool, toggle func() error) {
	actual := false
	if saved, ok := readAttachedVisibility(cfg.visibleFile); ok &&
		saved.pid == process.pid && saved.started == process.started {
		actual = saved.visible
	}
	if actual != visible {
		if err := toggle(); err != nil {
			fmt.Fprintf(os.Stderr, "bottombar: toggle attached bottom: %v\n", err)
			return
		}
	}
	body := []byte(fmt.Sprintf("%d %d %t\n", process.pid, process.started, visible))
	if err := replaceFile(cfg.visibleFile, body); err != nil {
		fmt.Fprintf(os.Stderr, "bottombar: record attached visibility: %v\n", err)
	}
}

// openBottomProcess binds a pidfd before validating the recorded generation.
// A PID+starttime record is the ownership established by startBottom, including
// while the child is still in exec. /proc cmdline and comm are not startup
// barriers: cmd.Start can return before they describe the new executable.
// Only legacy PID-only records need command matching before adoption.
//
// Uncertain observations retain the record and prevent a replacement launch;
// only confirmed exit, generation mismatch, or invalid identity discards it.
func openBottomProcess(cfg bottomBarConfig) (bottomProcess, bool) {
	b, err := os.ReadFile(cfg.pidFile)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "bottombar: read ownership: %v\n", err)
		}
		return bottomProcess{}, false
	}
	discard := func(reason string) {
		fmt.Fprintf(os.Stderr, "bottombar: discard ownership: %s\n", reason)
		if err := os.Remove(cfg.pidFile); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "bottombar: remove ownership: %v\n", err)
		}
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 || len(fields) > 2 {
		discard("malformed PID record")
		return bottomProcess{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		discard("invalid PID")
		return bottomProcess{}, false
	}
	var recorded uint64
	if len(fields) == 2 {
		recorded, err = strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			discard(fmt.Sprintf("pid=%d invalid starttime", pid))
			return bottomProcess{}, false
		}
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if err == unix.ESRCH {
			discard(fmt.Sprintf("pid=%d exited", pid))
		} else {
			fmt.Fprintf(os.Stderr, "bottombar: retain pid=%d: pidfd_open: %v\n", pid, err)
		}
		return bottomProcess{}, false
	}
	fail := func(reason string, stale bool) (bottomProcess, bool) {
		_ = unix.Close(pidfd)
		if stale {
			discard(fmt.Sprintf("pid=%d %s", pid, reason))
		} else {
			fmt.Fprintf(os.Stderr, "bottombar: retain pid=%d: %s\n", pid, reason)
		}
		return bottomProcess{}, false
	}
	exited, err := bottomProcessExited(pidfd)
	if err != nil {
		return fail(fmt.Sprintf("poll exit: %v", err), false)
	}
	if exited {
		return fail("exited", true)
	}
	started, err := processStartTime(pid)
	if err != nil {
		return fail(fmt.Sprintf("read starttime: %v", err), os.IsNotExist(err))
	}
	if len(fields) == 2 {
		if recorded != started {
			return fail("starttime changed", true)
		}
	} else {
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			return fail(fmt.Sprintf("read legacy comm: %v", err), os.IsNotExist(err))
		}
		if strings.TrimSpace(string(comm)) == "" {
			return fail("legacy comm unavailable during exec", false)
		}
		if strings.TrimSpace(string(comm)) != "waybar" {
			return fail("legacy comm does not match Waybar", true)
		}
		matches, err := bottomCommandMatches(cfg, pid)
		if err != nil {
			return fail(fmt.Sprintf("read legacy command: %v", err), os.IsNotExist(err))
		}
		if !matches {
			return fail("legacy command does not match bottom config", true)
		}
		// Upgrade only after validating a legacy record's command identity.
		if err := replaceFile(cfg.pidFile, []byte(fmt.Sprintf("%d %d\n", pid, started))); err != nil {
			fmt.Fprintf(os.Stderr, "bottombar: upgrade pidfile identity: %v\n", err)
		}
	}
	return bottomProcess{pid: pid, pidfd: pidfd, started: started}, true
}

// bottomMayBeRunning also blocks replacement when ownership could not be
// checked. Signal paths still require a validated generation and open pidfd.
func bottomMayBeRunning(cfg bottomBarConfig) bool {
	if bottomPID(cfg) > 0 {
		return true
	}
	_, err := os.Stat(cfg.pidFile)
	return !os.IsNotExist(err)
}

func bottomPID(cfg bottomBarConfig) int {
	process, ok := openBottomProcess(cfg)
	if !ok {
		return 0
	}
	_ = unix.Close(process.pidfd)
	return process.pid
}

func bottomProcessExited(pidfd int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return false, fmt.Errorf("unexpected pidfd events: %#x", fds[0].Revents)
		}
		return fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
	}
}

func bottomCommandMatches(cfg bottomBarConfig, pid int) (bool, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false, err
	}
	if len(b) == 0 {
		return false, fmt.Errorf("command line unavailable during exec")
	}
	args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && args[i+1] == cfg.waybarConfig {
			return true, nil
		}
	}
	return false, nil
}

func processStartTime(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return processStartTimeFromStat(string(b))
}

func processStartTimeFromStat(stat string) (uint64, error) {
	// comm is parenthesized and may contain spaces. Fields after its final ')'
	// begin at proc field 3 (state), making starttime/field 22 index 19 here.
	close := strings.LastIndexByte(stat, ')')
	if close < 0 {
		return 0, fmt.Errorf("malformed proc stat")
	}
	fields := strings.Fields(stat[close+1:])
	if len(fields) <= 19 {
		return 0, fmt.Errorf("short proc stat")
	}
	started, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse starttime: %w", err)
	}
	return started, nil
}

func mustFlock(path string) func() {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fail("bottombar: lock: %v", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		fail("bottombar: flock: %v", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}
}

func runtimeDir() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return x
	}
	return fmt.Sprintf("/tmp/run-%d", os.Getuid())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
