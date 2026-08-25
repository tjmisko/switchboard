package remotestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

const (
	DefaultRetryDelay = 2 * time.Second
	MaxRetryDelay     = 30 * time.Second

	DiagnosticCommand       = "command"
	DiagnosticStart         = "start"
	DiagnosticRead          = "read"
	DiagnosticInvalidFrame  = "invalid_frame"
	DiagnosticSchema        = "schema_mismatch"
	DiagnosticDuplicateHost = "duplicate_hostname"
	DiagnosticLocalHost     = "local_hostname"
	DiagnosticHostnameFlip  = "hostname_changed"
	DiagnosticDisconnected  = "disconnected"
)

// Process is the complete child-process seam. The production implementation is
// an exec.Cmd; unit tests use finite readers and controllable Wait calls.
type Process interface {
	StdoutPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
}

// CommandFactory creates, but does not start, one process for a destination.
// A worker never calls it again until the preceding process's Wait has returned.
type CommandFactory func(context.Context, string) (Process, error)

// RetryWaiter waits between non-overlapping attempts and returns false when the
// worker should stop. It is injectable so tests never sleep.
type RetryWaiter func(context.Context, time.Duration) bool

// Diagnostic is deliberately finite and content-free. Host is included only
// after a canonical hostname has passed frame validation; peer output and SSH
// stderr are never included.
type Diagnostic struct {
	Destination string
	Host        string
	Category    string
}

// ManagerConfig is static for a Manager's lifetime. Dynamic remote discovery is
// intentionally outside the first implementation.
type ManagerConfig struct {
	Destinations []string
	// LocalHostname reserves the client's own canonical namespace. A remote
	// source claiming it is rejected before any row is published.
	LocalHostname string
	Commands      CommandFactory
	ReadFrames    FrameReader
	RetryDelay    time.Duration
	WaitRetry     RetryWaiter
	MaxFrameBytes int
	// OnDiagnostic may be called concurrently by different destination workers.
	OnDiagnostic func(Diagnostic)
	// OnHostRemoved is the route/focus invalidation edge. Before it runs, the
	// manager tombstones the host so Snapshot and concurrent publications omit
	// it; after it returns, the owning disconnect publishes the removal. It may
	// be called concurrently for different hosts.
	OnHostRemoved func(string)
}

// Manager owns one sequential SSH worker per configured destination and an
// atomic, read-only latest-snapshot map keyed by canonical returned hostname.
// Hostname claims remain sticky across reconnects so duplicate SSH aliases
// cannot exchange ownership during a brief disconnect gap.
type Manager struct {
	destinations  []string
	localHostname string
	commands      CommandFactory
	readFrames    FrameReader
	retryDelay    time.Duration
	waitRetry     RetryWaiter
	maxFrameBytes int
	onDiagnostic  func(Diagnostic)
	onHostRemoved func(string)

	mu          sync.RWMutex
	live        map[string]state.Snapshot
	removing    map[string]struct{}
	hostOwners  map[string]string
	destHosts   map[string]string
	subscribers map[chan map[string]state.Snapshot]struct{}
	ran         bool
}

// NewManager validates the complete static configuration before any worker or
// child process starts.
func NewManager(config ManagerConfig) (*Manager, error) {
	var localHostname string
	if config.LocalHostname != "" {
		var err error
		localHostname, err = CanonicalHostname(config.LocalHostname)
		if err != nil {
			return nil, fmt.Errorf("invalid local hostname: %w", err)
		}
	}
	seen := make(map[string]struct{}, len(config.Destinations))
	destinations := make([]string, len(config.Destinations))
	for i, destination := range config.Destinations {
		if err := validateDestination(destination); err != nil {
			return nil, err
		}
		if _, duplicate := seen[destination]; duplicate {
			return nil, fmt.Errorf("duplicate SSH destination %q", destination)
		}
		seen[destination] = struct{}{}
		destinations[i] = destination
	}
	limit, err := frameLimit(config.MaxFrameBytes)
	if err != nil {
		return nil, err
	}
	delay := config.RetryDelay
	if delay == 0 {
		delay = DefaultRetryDelay
	}
	if delay < 0 || delay > MaxRetryDelay {
		return nil, fmt.Errorf("retry delay must be between 0 and %s", MaxRetryDelay)
	}
	commands := config.Commands
	if commands == nil {
		commands = newSSHCommand
	}
	readFrames := config.ReadFrames
	if readFrames == nil {
		readFrames = ReadFrames
	}
	waitRetry := config.WaitRetry
	if waitRetry == nil {
		waitRetry = waitForRetry
	}
	return &Manager{
		destinations:  destinations,
		localHostname: localHostname,
		commands:      commands,
		readFrames:    readFrames,
		retryDelay:    delay,
		waitRetry:     waitRetry,
		maxFrameBytes: limit,
		onDiagnostic:  config.OnDiagnostic,
		onHostRemoved: config.OnHostRemoved,
		live:          make(map[string]state.Snapshot),
		removing:      make(map[string]struct{}),
		hostOwners:    make(map[string]string),
		destHosts:     make(map[string]string),
		subscribers:   make(map[chan map[string]state.Snapshot]struct{}),
	}, nil
}

func validateDestination(destination string) error {
	if destination == "" || destination != strings.TrimSpace(destination) || len(destination) > 512 || strings.HasPrefix(destination, "-") {
		return fmt.Errorf("invalid SSH destination")
	}
	for i := 0; i < len(destination); i++ {
		if destination[i] <= ' ' || destination[i] == 0x7f {
			return fmt.Errorf("invalid SSH destination")
		}
	}
	return nil
}

type execProcess struct{ *exec.Cmd }

func newSSHCommand(ctx context.Context, destination string) (Process, error) {
	cmd := exec.CommandContext(ctx, "ssh",
		"-n", "-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ClearAllForwardings=yes",
		destination, "switchboard-ctl", "remote-stream",
	)
	cmd.Env = environmentWith(os.Environ(), "SSH_ASKPASS_REQUIRE", "never")
	cmd.WaitDelay = 5 * time.Second
	return execProcess{cmd}, nil
}

func environmentWith(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, variable := range environment {
		if !strings.HasPrefix(variable, prefix) {
			result = append(result, variable)
		}
	}
	return append(result, prefix+value)
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Run starts all configured workers and blocks until ctx stops them. A Manager
// is single-use, which keeps destination-to-host claims unambiguous.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	if m.ran {
		m.mu.Unlock()
		return ErrManagerAlreadyRun
	}
	m.ran = true
	m.mu.Unlock()

	var workers sync.WaitGroup
	for _, destination := range m.destinations {
		workers.Add(1)
		go func(destination string) {
			defer workers.Done()
			m.runWorker(ctx, destination)
		}(destination)
	}
	workers.Wait()
	return nil
}

func (m *Manager) runWorker(ctx context.Context, destination string) {
	for ctx.Err() == nil {
		m.runAttempt(ctx, destination)
		if ctx.Err() != nil || !m.waitRetry(ctx, m.retryDelay) {
			return
		}
	}
}

func (m *Manager) runAttempt(parent context.Context, destination string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	process, err := m.commands(ctx, destination)
	if err != nil {
		m.diagnose(destination, "", DiagnosticCommand)
		return
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		m.diagnose(destination, "", DiagnosticCommand)
		return
	}
	if err := process.Start(); err != nil {
		_ = stdout.Close()
		m.diagnose(destination, "", DiagnosticStart)
		return
	}

	readDone := make(chan struct{})
	watchResult := make(chan bool, 1)
	go func() {
		select {
		case <-ctx.Done():
			_ = stdout.Close()
			watchResult <- true
		case <-readDone:
			watchResult <- false
		}
	}()
	readErr := m.readFrames(stdout, m.maxFrameBytes, func(frame Frame) error {
		return m.accept(destination, frame)
	})
	close(readDone)
	cancel()
	if closedByWatcher := <-watchResult; !closedByWatcher {
		_ = stdout.Close()
	}
	host, _ := m.removeLive(destination)
	// The host is already non-actionable and absent before Wait: an SSH child
	// can be slow to reap, but its dead stream must never leave stale live rows.
	// The worker still waits below before returning to the retry loop, so child
	// processes never overlap.
	waitErr := process.Wait()
	if parent.Err() != nil {
		return
	}
	m.diagnoseAttempt(destination, host, readErr, waitErr)
}

func (m *Manager) accept(destination string, frame Frame) error {
	canonical, err := CanonicalHostname(frame.Host)
	if err != nil || canonical != frame.Host {
		return ErrInvalidFrame
	}
	if err := validateSnapshot(frame.Snapshot); err != nil {
		return err
	}
	if m.localHostname != "" && frame.Host == m.localHostname {
		return ErrLocalHostname
	}
	m.mu.Lock()
	claimedHost := m.destHosts[destination]
	if claimedHost != "" && claimedHost != frame.Host {
		m.mu.Unlock()
		return ErrHostnameChanged
	}
	if owner := m.hostOwners[frame.Host]; owner != "" && owner != destination {
		m.mu.Unlock()
		return ErrDuplicateHost
	}
	if claimedHost == "" {
		m.destHosts[destination] = frame.Host
		m.hostOwners[frame.Host] = destination
	}
	// Only this hostname's owning sequential worker can reach this point. A
	// reconnect adopts a fresh full frame and makes the host visible again.
	delete(m.removing, frame.Host)
	m.live[frame.Host] = frame.Snapshot
	m.publishLocked()
	m.mu.Unlock()
	return nil
}

func (m *Manager) removeLive(destination string) (string, bool) {
	m.mu.Lock()
	host := m.destHosts[destination]
	if host == "" || m.hostOwners[host] != destination {
		m.mu.Unlock()
		return host, false
	}
	if _, exists := m.live[host]; !exists {
		m.mu.Unlock()
		return host, false
	}
	if _, removing := m.removing[host]; removing {
		m.mu.Unlock()
		return host, false
	}
	// Hide the host from every current Snapshot before releasing the lock. A
	// different destination may publish while the callback below runs; its full
	// map must not carry this dead stream back into route liveness.
	m.removing[host] = struct{}{}
	m.mu.Unlock()

	// Invalidate action routes outside the lock so the callback may read
	// Manager.Snapshot or update the aggregate view. Snapshot already omits the
	// tombstoned host, so even a cached UI request fails its current-view guard.
	if m.onHostRemoved != nil {
		m.onHostRemoved(host)
	}

	m.mu.Lock()
	if m.hostOwners[host] != destination {
		delete(m.removing, host)
		m.mu.Unlock()
		return host, false
	}
	if _, exists := m.live[host]; !exists {
		delete(m.removing, host)
		m.mu.Unlock()
		return host, false
	}
	delete(m.live, host)
	delete(m.removing, host)
	m.publishLocked()
	m.mu.Unlock()
	return host, true
}

func (m *Manager) diagnoseAttempt(destination, host string, readErr, waitErr error) {
	switch {
	case errors.Is(readErr, ErrDuplicateHost):
		m.diagnose(destination, "", DiagnosticDuplicateHost)
	case errors.Is(readErr, ErrLocalHostname):
		m.diagnose(destination, m.localHostname, DiagnosticLocalHost)
	case errors.Is(readErr, ErrHostnameChanged):
		m.diagnose(destination, host, DiagnosticHostnameFlip)
	case errors.Is(readErr, ErrSchemaMismatch):
		m.diagnose(destination, host, DiagnosticSchema)
	case errors.Is(readErr, ErrInvalidFrame), errors.Is(readErr, ErrFrameTooLarge):
		m.diagnose(destination, host, DiagnosticInvalidFrame)
	case readErr != nil && !errors.Is(readErr, io.EOF):
		m.diagnose(destination, host, DiagnosticRead)
	case waitErr != nil:
		m.diagnose(destination, host, DiagnosticDisconnected)
	default:
		m.diagnose(destination, host, DiagnosticDisconnected)
	}
}

func (m *Manager) diagnose(destination, host, category string) {
	if m.onDiagnostic != nil {
		m.onDiagnostic(Diagnostic{Destination: destination, Host: host, Category: category})
	}
}

// Snapshot returns a detached, point-in-time full replacement map. Absence of
// a hostname means its SSH stream is not currently live.
func (m *Manager) Snapshot() map[string]state.Snapshot {
	m.mu.RLock()
	view := m.snapshotLocked()
	m.mu.RUnlock()
	return cloneSnapshotMap(view)
}

// Subscribe registers for full replacement host maps after live state changes.
// The bounded channel coalesces toward the newest value when its consumer
// lags; every value is complete, so skipped intermediate maps cost no
// correctness. Subscribe first and then call Snapshot for a race-free initial
// view. A value already queued before that Snapshot may precede the initial
// view, so consumers which require monotonic current state should treat channel
// receives as notifications and re-read Snapshot (as federation does).
func (m *Manager) Subscribe() (<-chan map[string]state.Snapshot, func()) {
	ch := make(chan map[string]state.Snapshot, 4)
	m.mu.Lock()
	m.subscribers[ch] = struct{}{}
	m.mu.Unlock()
	cancel := func() {
		m.mu.Lock()
		if _, exists := m.subscribers[ch]; exists {
			delete(m.subscribers, ch)
			close(ch)
		}
		m.mu.Unlock()
	}
	return ch, cancel
}

func (m *Manager) publishLocked() {
	if len(m.subscribers) == 0 {
		return
	}
	view := cloneSnapshotMap(m.snapshotLocked())
	for subscriber := range m.subscribers {
		select {
		case subscriber <- view:
		default:
			// Every value is a complete replacement and there is no heartbeat.
			// Evict one stale value and enqueue the newest so a final disconnect
			// cannot remain invisible forever to a temporarily slow consumer.
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- view:
			default:
			}
		}
	}
}

func (m *Manager) snapshotLocked() map[string]state.Snapshot {
	view := make(map[string]state.Snapshot, len(m.live))
	for host, snapshot := range m.live {
		if _, removing := m.removing[host]; removing {
			continue
		}
		view[host] = snapshot
	}
	return view
}

func cloneSnapshotMap(source map[string]state.Snapshot) map[string]state.Snapshot {
	clone := make(map[string]state.Snapshot, len(source))
	for host, snapshot := range source {
		// Remote snapshots arrived through JSON, so a wire round trip is the
		// smallest reliable deep copy of nested provider graph slices/pointers.
		// All state fields are JSON-representable; retain the original only if a
		// future field violates that invariant, rather than dropping a live host.
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			clone[host] = snapshot
			continue
		}
		var detached state.Snapshot
		if err := json.Unmarshal(encoded, &detached); err != nil {
			clone[host] = snapshot
			continue
		}
		clone[host] = detached
	}
	return clone
}
