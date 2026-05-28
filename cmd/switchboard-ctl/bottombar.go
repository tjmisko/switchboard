package main

// The bottombar subcommand owns the lifecycle of the bottom ("claude") waybar
// process. It enforces a single invariant:
//
//	bottom bar runs  <=>  (top bar visible)  AND  (>=1 claude session)
//
// The bottom bar's visibility primitive is process existence — we literally
// start and stop the `waybar -c claude.jsonc` process — so there is no toggle
// state to desync. The running process IS the truth, which is what keeps the
// two bars in lockstep with no alternation under repeated F8 presses.
//
// Two inputs drive the invariant, each owned by a different actor:
//
//	top visible : the F8 master toggle, recorded as the presence/absence of a
//	              marker file (absent => visible). Owned by hypr-float-center.
//	sessions    : the switchboard daemon's session count.
//
// `bottombar watch` reacts to session changes (subscribe stream + safety
// ticker). `bottombar reconcile` is the one-shot the F8 script calls after it
// flips the master toggle, so the bottom bar follows the top in lockstep. Both
// funnel through reconcile under a flock, so they never race each other.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"golang.org/x/sys/unix"
)

type bottomBarConfig struct {
	socketPath   string
	marker       string // master-visibility marker; present => top bar hidden
	pidFile      string
	lockFile     string
	waybarConfig string
}

func bottomBarConfigDefault(socketPath string) bottomBarConfig {
	run := runtimeDir()
	home, _ := os.UserHomeDir()
	return bottomBarConfig{
		socketPath:   socketPath,
		marker:       envOr("SWITCHBOARD_WAYBAR_MARKER", "/tmp/hypr-float-center/waybar-hidden"),
		pidFile:      filepath.Join(run, "switchboard", "bottom-waybar.pid"),
		lockFile:     filepath.Join(run, "switchboard", "bottombar.lock"),
		waybarConfig: envOr("SWITCHBOARD_BOTTOM_CONFIG", filepath.Join(home, ".config", "waybar", "claude.jsonc")),
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
	case "stop":
		unlock := mustFlock(cfg.lockFile)
		ensureStopped(cfg)
		unlock()
	default:
		fail("bottombar: unknown subcommand %q (want reconcile|watch|stop)", sub)
	}
}

// shouldRun is the bottom-bar invariant, distilled to a pure decision: the
// bottom bar runs iff the top bar is visible AND at least one claude session
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
	defer unlock()

	visible := topVisible(cfg)
	if !visible {
		// Master toggle is off: the bottom bar must not exist regardless of
		// session count. We can decide this without the daemon.
		ensureStopped(cfg)
		return
	}
	count, ok := sessionCount(cfg.socketPath)
	if !ok {
		// Daemon unreachable — we cannot know the session count, so leave the
		// bottom bar in whatever state it is. Better than flapping.
		return
	}
	setBottom(cfg, shouldRun(visible, count))
}

// reconcileWith is reconcile when the caller already knows the session count
// (e.g. from a subscribe snapshot), avoiding a redundant daemon round-trip.
func reconcileWith(cfg bottomBarConfig, count int) {
	unlock := mustFlock(cfg.lockFile)
	defer unlock()
	setBottom(cfg, shouldRun(topVisible(cfg), count))
}

func setBottom(cfg bottomBarConfig, run bool) {
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
	// Reap bottom-bar processes we (or an F8 one-shot) start. We launch them
	// detached with Release() and never Wait(), so when one is killed it would
	// linger as a zombie under us — its parent — until we reap it. A one-shot
	// reconcile cannot reap a child of ours, so the responsibility lands here.
	go reapChildren()

	go func() {
		for range time.Tick(3 * time.Second) {
			reconcile(cfg)
		}
	}()

	for {
		streamSnapshots(cfg)
		// Connection dropped. Reconcile once (the daemon may be restarting),
		// then retry. The ticker keeps things honest in the meantime.
		reconcile(cfg)
		time.Sleep(2 * time.Second)
	}
}

// streamSnapshots subscribes and reconciles on each snapshot until the
// connection drops, then returns.
func streamSnapshots(cfg bottomBarConfig) {
	c, err := rpc.Dial(cfg.socketPath)
	if err != nil {
		return
	}
	defer c.Close()
	if err := c.Send(rpc.Request{Cmd: "subscribe"}); err != nil {
		return
	}
	for {
		var resp rpc.Response
		if err := c.Recv(&resp); err != nil {
			return
		}
		if resp.Snapshot == nil {
			continue
		}
		reconcileWith(cfg, len(resp.Snapshot.Sessions))
	}
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
	if err := c.Send(rpc.Request{Cmd: "list"}); err != nil {
		return 0, false
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		return 0, false
	}
	if resp.Snapshot == nil {
		return 0, true
	}
	return len(resp.Snapshot.Sessions), true
}

// ensureStarted launches the bottom waybar if it is not already running.
func ensureStarted(cfg bottomBarConfig) {
	if bottomPID(cfg) > 0 {
		return
	}
	if err := startBottom(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "bottombar: start: %v\n", err)
	}
}

// ensureStopped kills the bottom waybar (and its module subprocesses) if it is
// running, and clears the pidfile.
func ensureStopped(cfg bottomBarConfig) {
	if pid := bottomPID(cfg); pid > 0 {
		// Negative pid targets the whole process group. The bottom waybar is a
		// session/group leader (Setsid below), so this also reaps the
		// switchboard-waybar slot subprocesses — no orphans writing to a dead pipe.
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	_ = os.Remove(cfg.pidFile)
}

// startBottom spawns `waybar -c <claude config>` detached into its own session
// so it survives this process, and records its pid.
func startBottom(cfg bottomBarConfig) error {
	cmd := exec.Command("waybar", "-c", cfg.waybarConfig)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if dn, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = dn, dn, dn
		defer dn.Close()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return os.WriteFile(cfg.pidFile, []byte(strconv.Itoa(pid)), 0o644)
}

// bottomPID returns the live pid of the bottom waybar, or 0 if it is not
// running. It verifies the recorded pid is still a waybar (guarding against pid
// reuse) and cleans up a stale pidfile.
func bottomPID(cfg bottomBarConfig) int {
	b, err := os.ReadFile(cfg.pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		_ = os.Remove(cfg.pidFile)
		return 0
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil || strings.TrimSpace(string(comm)) != "waybar" {
		_ = os.Remove(cfg.pidFile)
		return 0
	}
	return pid
}

// reapChildren blocks on any child state change and reaps it, so killed
// bottom-bar processes do not pile up as zombies. When we have no children,
// Wait4 returns ECHILD; we sleep briefly to avoid spinning.
func reapChildren() {
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			time.Sleep(time.Second)
		}
	}
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
