package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/testsupport"
	"github.com/tjmisko/switchboard/internal/waybarchip"
)

func TestConfigureBottomCommandIOPreservesDiagnostics(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	cmd := exec.Command("waybar")
	configureBottomCommandIO(cmd, devNull)

	if cmd.Stdin != devNull {
		t.Error("waybar stdin should be connected to /dev/null")
	}
	if cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr {
		t.Error("waybar diagnostics should remain connected to the watcher")
	}
}

// §10.1 shouldRun — the bottom-bar invariant's pure core. All four F8
// truth-table cases.
func TestShouldRun(t *testing.T) {
	tests := []struct {
		name       string
		topVisible bool
		count      int
		want       bool
	}{
		{"top hidden, no sessions", false, 0, false},
		{"top hidden, sessions present", false, 3, false},
		{"top visible, no sessions", true, 0, false},
		{"top visible, sessions present", true, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRun(tt.topVisible, tt.count); got != tt.want {
				t.Errorf("shouldRun(%v, %d) = %v, want %v", tt.topVisible, tt.count, got, tt.want)
			}
		})
	}
}

// §10.2 topVisible / bottomPID / envOr — seed cases (0.4 expands). Uses the
// harness's temp-file builders for the master-visibility marker and the stale
// pidfile.
func TestTopVisible(t *testing.T) {
	dir := t.TempDir()
	cfg := bottomBarConfig{marker: filepath.Join(dir, "waybar-hidden")}

	if !topVisible(cfg) {
		t.Error("topVisible should be true when the marker is absent")
	}
	testsupport.Touch(t, cfg.marker)
	if topVisible(cfg) {
		t.Error("topVisible should be false when the marker is present")
	}
}

func TestBottomPIDCleansStalePidfile(t *testing.T) {
	dir := t.TempDir()
	cfg := bottomBarConfig{pidFile: filepath.Join(dir, "bottom.pid")}

	// Our own pid is live but its comm is the test binary, not "waybar", so the
	// pid-reuse guard must reject it and remove the stale pidfile.
	testsupport.WritePIDFile(t, cfg.pidFile, os.Getpid())
	if got := bottomPID(cfg); got != 0 {
		t.Errorf("bottomPID = %d, want 0 (comm guard)", got)
	}
	if _, err := os.Stat(cfg.pidFile); !os.IsNotExist(err) {
		t.Error("stale pidfile was not cleaned up")
	}
}

func TestProcessStartTimeParsesCommWithSpaces(t *testing.T) {
	// Fields after ')' start at proc field 3. Eighteen zeroes place 9876 at
	// field 22 (starttime), while the parenthesized comm deliberately has spaces.
	stat := "42 (bottom waybar) S " + strings.Repeat("0 ", 18) + "9876 0 0\n"
	got, err := processStartTimeFromStat(stat)
	if err != nil {
		t.Fatal(err)
	}
	if got != 9876 {
		t.Fatalf("starttime = %d, want 9876", got)
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("SWITCHBOARD_TEST_ENVOR", "set-value")
	if got := envOr("SWITCHBOARD_TEST_ENVOR", "fallback"); got != "set-value" {
		t.Errorf("envOr (set) = %q, want set-value", got)
	}
	if got := envOr("SWITCHBOARD_TEST_ENVOR_MISSING", "fallback"); got != "fallback" {
		t.Errorf("envOr (unset) = %q, want fallback", got)
	}
}

func TestUnsupportedRPCCommand(t *testing.T) {
	if !unsupportedRPCCommand("unknown cmd: subscribe-all", "subscribe-all") {
		t.Error("exact legacy-daemon response should enable the local fallback")
	}
	for _, message := range []string{
		"permission denied",
		"unknown cmd: subscribe",
		"unknown cmd: subscribe-all-extra",
	} {
		if unsupportedRPCCommand(message, "subscribe-all") {
			t.Errorf("unexpected fallback for %q", message)
		}
	}
}

// stubOps is an in-memory bottomBarOps: it tracks a running flag and counts
// start/stop calls, so the reconcile orchestration can be driven without
// launching a real waybar.
type stubOps struct {
	running bool
	starts  int
	stops   int
}

func (s *stubOps) ops() bottomBarOps {
	return bottomBarOps{
		isRunning: func(bottomBarConfig) bool { return s.running },
		start:     func(bottomBarConfig) error { s.running = true; s.starts++; return nil },
		stop:      func(bottomBarConfig) { s.running = false; s.stops++ },
	}
}

func newStubConfig(t *testing.T, stub *stubOps) bottomBarConfig {
	dir := t.TempDir()
	return bottomBarConfig{
		marker:     filepath.Join(dir, "waybar-hidden"),
		lockFile:   filepath.Join(dir, "bottombar.lock"),
		socketPath: filepath.Join(dir, "nonexistent.sock"),
		ops:        stub.ops(),
	}
}

// §11 reconcileWith — the four F8 truth-table cases end-to-end (marker drives
// topVisible). (top-hidden, bottom-present) never arises: hidden always stops.
func TestReconcileWithF8TruthTable(t *testing.T) {
	cases := []struct {
		name        string
		topHidden   bool
		count       int
		wantRunning bool
	}{
		{"hidden, no sessions", true, 0, false},
		{"hidden, sessions present", true, 3, false},
		{"visible, no sessions", false, 0, false},
		{"visible, sessions present", false, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubOps{}
			cfg := newStubConfig(t, stub)
			if c.topHidden {
				testsupport.Touch(t, cfg.marker)
			}
			reconcileWith(cfg, c.count)
			if stub.running != c.wantRunning {
				t.Errorf("running = %v, want %v", stub.running, c.wantRunning)
			}
		})
	}
}

// §11 reconcile — top hidden stops the bar without dialing the daemon.
func TestReconcileTopHiddenStopsWithoutDaemon(t *testing.T) {
	stub := &stubOps{running: true}
	cfg := newStubConfig(t, stub)
	testsupport.Touch(t, cfg.marker) // hidden

	reconcile(cfg)

	if stub.running {
		t.Error("bottom bar should be stopped when top is hidden")
	}
	if stub.stops == 0 {
		t.Error("stop should have been called")
	}
}

// §11 reconcile — when the daemon is unreachable (top visible, no socket) the
// bar is left in whatever state it is, no flap.
func TestReconcileDaemonUnreachableLeavesAsIs(t *testing.T) {
	stub := &stubOps{running: true}
	cfg := newStubConfig(t, stub) // marker absent => top visible; socket does not exist

	reconcile(cfg)

	if !stub.running {
		t.Error("running bar must be left as-is when the daemon is unreachable")
	}
	if stub.starts != 0 || stub.stops != 0 {
		t.Errorf("no start/stop expected (got starts=%d stops=%d)", stub.starts, stub.stops)
	}
}

// §11 ensureStarted/ensureStopped idempotence.
func TestEnsureStartStopIdempotent(t *testing.T) {
	stub := &stubOps{}
	cfg := bottomBarConfig{ops: stub.ops()}

	ensureStarted(cfg)
	ensureStarted(cfg) // already running -> no second launch
	if stub.starts != 1 {
		t.Errorf("start called %d times, want 1 (idempotent)", stub.starts)
	}

	ensureStopped(cfg)
	if stub.running {
		t.Error("bar should be stopped")
	}
	ensureStopped(cfg) // already stopped -> no error/panic
}

func TestRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := runtimeDir(); got != "/run/user/1000" {
		t.Errorf("runtimeDir (set) = %q, want /run/user/1000", got)
	}

	t.Setenv("XDG_RUNTIME_DIR", "")
	got := runtimeDir()
	want := fmt.Sprintf("/tmp/run-%d", os.Getuid())
	if got != want {
		t.Errorf("runtimeDir (unset) = %q, want %q", got, want)
	}
}

func TestSlotPublisherReplacesBeforeSignalAndDedupesExactBytes(t *testing.T) {
	dir := t.TempDir()
	publisher := newSlotPublisher(dir, 2)
	var replaced []int
	var signaled []int
	publisher.replace = func(path string, body []byte) error {
		var slot int
		if _, err := fmt.Sscanf(filepath.Base(path), "slot-%d.json", &slot); err != nil {
			return err
		}
		if len(signaled) != 0 {
			t.Fatal("slot signaled before every replacement completed")
		}
		replaced = append(replaced, slot)
		return os.WriteFile(path, body, 0o600)
	}
	publisher.signal = func(pid, slot int) error {
		if pid != 4242 {
			t.Fatalf("signal pid = %d, want 4242", pid)
		}
		if !slices.Contains(replaced, slot) {
			t.Fatalf("slot %d signaled before replacement", slot)
		}
		signaled = append(signaled, slot)
		return nil
	}

	outputs := []waybarchip.Output{
		{Text: "one", Class: []string{"working"}},
		{Text: "two", Class: []string{"idle"}},
	}
	if err := publisher.stage(outputs); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(replaced, []int{0, 1}) {
		t.Fatalf("replaced = %v, want [0 1]", replaced)
	}
	publisher.flush(4242)
	if !slices.Equal(signaled, []int{0, 1}) {
		t.Fatalf("signaled = %v, want [0 1]", signaled)
	}

	replaced = nil
	signaled = nil
	if err := publisher.stage(outputs); err != nil {
		t.Fatal(err)
	}
	publisher.flush(4242)
	if len(replaced) != 0 || len(signaled) != 0 {
		t.Fatalf("byte-identical output replaced=%v signaled=%v, want neither", replaced, signaled)
	}
}

func TestSlotPublisherWaitsForExactWaybarReadyPIDThenCatchesUp(t *testing.T) {
	dir := t.TempDir()
	cfg := bottomBarConfig{readyFile: filepath.Join(dir, "bottom-waybar.ready")}
	publisher := newSlotPublisher(dir, 1)
	publisher.replace = func(path string, body []byte) error {
		return os.WriteFile(path, body, 0o600)
	}
	var signals int
	publisher.signal = func(pid, slot int) error {
		signals++
		return nil
	}
	if err := publisher.stage([]waybarchip.Output{{Text: "new", Class: []string{"working"}}}); err != nil {
		t.Fatal(err)
	}

	if flushSlotsForProcess(cfg, publisher, 4242, 4242) {
		t.Fatal("flush reported complete before Waybar declared readiness")
	}
	if signals != 0 || !publisher.hasDirty() {
		t.Fatalf("before ready: signals=%d dirty=%v, want 0/true", signals, publisher.hasDirty())
	}
	if err := os.WriteFile(cfg.readyFile, []byte("1111\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if flushSlotsForProcess(cfg, publisher, 4242, 4242) {
		t.Fatal("stale ready PID must not authorize signals")
	}
	if signals != 0 || !publisher.hasDirty() {
		t.Fatalf("stale ready: signals=%d dirty=%v, want 0/true", signals, publisher.hasDirty())
	}
	if err := os.WriteFile(cfg.readyFile, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !flushSlotsForProcess(cfg, publisher, 4242, 4242) {
		t.Fatal("matching ready PID did not flush the startup catch-up")
	}
	if signals != 1 || publisher.hasDirty() {
		t.Fatalf("after ready: signals=%d dirty=%v, want 1/false", signals, publisher.hasDirty())
	}
}

func TestSlotPublisherRetriesFailedSignals(t *testing.T) {
	publisher := newSlotPublisher(t.TempDir(), 1)
	publisher.replace = func(string, []byte) error { return nil }
	attempts := 0
	publisher.signal = func(int, int) error {
		attempts++
		if attempts == 1 {
			return errors.New("not ready")
		}
		return nil
	}
	if err := publisher.stage([]waybarchip.Output{{Text: "one", Class: []string{"working"}}}); err != nil {
		t.Fatal(err)
	}
	publisher.flush(42)
	if !publisher.hasDirty() {
		t.Fatal("failed signal discarded the pending update")
	}
	publisher.flush(42)
	if publisher.hasDirty() || attempts != 2 {
		t.Fatalf("retry dirty=%v attempts=%d, want false/2", publisher.hasDirty(), attempts)
	}
}

func TestSlotPublisherSuppressesPartialGenerationUntilFullRetry(t *testing.T) {
	publisher := newSlotPublisher(t.TempDir(), 2)
	failSecond := false
	replaced := make([]int, 0, 4)
	publisher.replace = func(path string, _ []byte) error {
		var slot int
		if _, err := fmt.Sscanf(filepath.Base(path), "slot-%d.json", &slot); err != nil {
			return err
		}
		replaced = append(replaced, slot)
		if slot == 1 && failSecond {
			return errors.New("disk full")
		}
		return nil
	}
	var signals int
	publisher.signal = func(int, int) error { signals++; return nil }
	initial := []waybarchip.Output{{Text: "a"}, {Text: "a"}}
	if err := publisher.stage(initial); err != nil {
		t.Fatal(err)
	}
	publisher.flush(9)
	replaced, signals = nil, 0

	failSecond = true
	outputs := []waybarchip.Output{{Text: "b"}, {Text: "b"}}
	if err := publisher.stage(outputs); err == nil {
		t.Fatal("partial stage unexpectedly succeeded")
	}
	publisher.flush(9)
	if signals != 0 || !publisher.blocked {
		t.Fatalf("partial generation signals=%d blocked=%v, want 0/true", signals, publisher.blocked)
	}

	failSecond = false
	// Slot zero returns to the last committed value while slot one advances.
	// Comparing only with last would skip slot zero and leave its failed-prefix
	// "b" on disk forever; a blocked retry must force both replacements.
	next := []waybarchip.Output{{Text: "a"}, {Text: "c"}}
	if err := publisher.stage(next); err != nil {
		t.Fatal(err)
	}
	publisher.flush(9)
	if signals != 2 || publisher.blocked || publisher.hasDirty() {
		t.Fatalf("full retry signals=%d blocked=%v dirty=%v, want 2/false/false", signals, publisher.blocked, publisher.hasDirty())
	}
	if !slices.Equal(replaced, []int{0, 1, 0, 1}) {
		t.Fatalf("replacement attempts = %v, want full [0 1] retry", replaced)
	}
}

type scriptedBottomStream struct {
	responses []rpc.Response
	next      int
}

func (s *scriptedBottomStream) Send(rpc.Request) error { return nil }
func (s *scriptedBottomStream) Recv(resp *rpc.Response) error {
	if s.next == len(s.responses) {
		return io.EOF
	}
	*resp = s.responses[s.next]
	s.next++
	return nil
}

func TestReceiveBottomSnapshotsDrainsNewestReplacementBeforeEOF(t *testing.T) {
	first := state.Snapshot{Sessions: []state.Session{{PID: 1}}}
	final := state.Snapshot{Sessions: []state.Session{{PID: 2}}}
	stream := &scriptedBottomStream{responses: []rpc.Response{
		{Snapshot: &first},
		{Snapshot: &final},
	}}
	snapshots := make(chan state.Snapshot, 1)
	go receiveBottomSnapshots(stream, "subscribe-all", snapshots)

	var got []state.Snapshot
	for snapshot := range snapshots {
		got = append(got, snapshot)
	}
	if len(got) == 0 || got[len(got)-1].Sessions[0].PID != 2 {
		t.Fatalf("drained snapshots = %+v, want final PID 2", got)
	}
}

func TestWaybarReadinessPollingIsExponentialAndBounded(t *testing.T) {
	want := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
		200 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond}
	for attempt, delay := range want {
		got, ok := nextWaybarReadyDelay(attempt)
		if !ok || got != delay {
			t.Fatalf("attempt %d = %v/%v, want %v/true", attempt, got, ok, delay)
		}
	}
	if delay, ok := nextWaybarReadyDelay(len(want)); ok || delay != 0 {
		t.Fatalf("poll after bounded attempts = %v/%v, want 0/false", delay, ok)
	}
}

func BenchmarkSlotPublisherStageAllChanged(b *testing.B) {
	publisher := newSlotPublisher(b.TempDir(), bottomBarSlots)
	outputs := make([]waybarchip.Output, bottomBarSlots)
	for i := range outputs {
		outputs[i] = waybarchip.Output{Text: "a", Class: []string{"working"}}
	}
	b.ReportAllocs()
	b.ResetTimer()
	flip := false
	for b.Loop() {
		flip = !flip
		for i := range outputs {
			if flip {
				outputs[i].Text = "a"
			} else {
				outputs[i].Text = "b"
			}
		}
		if err := publisher.stage(outputs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSlotPublisherStageUnchanged(b *testing.B) {
	publisher := newSlotPublisher(b.TempDir(), bottomBarSlots)
	outputs := make([]waybarchip.Output, bottomBarSlots)
	for i := range outputs {
		outputs[i] = waybarchip.Output{Text: "same", Class: []string{"working"}}
	}
	if err := publisher.stage(outputs); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := publisher.stage(outputs); err != nil {
			b.Fatal(err)
		}
	}
}
