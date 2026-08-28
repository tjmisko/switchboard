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
	DefaultRetryDelay      = 2 * time.Second
	MaxRetryDelay          = 30 * time.Second
	defaultRecoveryTimeout = 10 * time.Second

	DiagnosticCommand       = "command"
	DiagnosticStart         = "start"
	DiagnosticRead          = "read"
	DiagnosticInvalidFrame  = "invalid_frame"
	DiagnosticSchema        = "schema_mismatch"
	DiagnosticDuplicateHost = "duplicate_hostname"
	DiagnosticLocalHost     = "local_hostname"
	DiagnosticHostnameFlip  = "hostname_changed"
	DiagnosticDisconnected  = "disconnected"
	// DiagnosticReconnecting means a previously live stream ended, but its rows
	// remain visible while the owning worker makes one confirmation attempt.
	DiagnosticReconnecting = "reconnecting"
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
	// it; after it returns, the owning worker publishes the removal. A transport
	// loss does not call it until one bounded reconnect attempt fails to produce
	// a valid frame. It may be called concurrently for different hosts.
	OnHostRemoved func(string)
}

// Manager owns one sequential SSH worker per configured destination and an
// atomic, read-only latest-snapshot map keyed by canonical returned hostname.
// Hostname claims remain sticky across reconnects so duplicate SSH aliases
// cannot exchange ownership during a brief disconnect gap.
type Manager struct {
	destinations    []string
	localHostname   string
	commands        CommandFactory
	readFrames      FrameReader
	retryDelay      time.Duration
	recoveryTimeout time.Duration
	waitRetry       RetryWaiter
	maxFrameBytes   int
	onDiagnostic    func(Diagnostic)
	onHostRemoved   func(string)

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
		destinations:    destinations,
		localHostname:   localHostname,
		commands:        commands,
		readFrames:      readFrames,
		retryDelay:      delay,
		recoveryTimeout: defaultRecoveryTimeout,
		waitRetry:       waitRetry,
		maxFrameBytes:   limit,
		onDiagnostic:    config.OnDiagnostic,
		onHostRemoved:   config.OnHostRemoved,
		live:            make(map[string]state.Snapshot),
		removing:        make(map[string]struct{}),
		hostOwners:      make(map[string]string),
		destHosts:       make(map[string]string),
		subscribers:     make(map[chan map[string]state.Snapshot]struct{}),
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
	recovering := false
	for ctx.Err() == nil {
		recovering = m.runAttempt(ctx, destination, recovering)
		if ctx.Err() != nil {
			return
		}
		if !m.waitRetry(ctx, m.retryDelay) {
			// A non-cancellation waiter refusal means there will be no confirmation
			// attempt. Do not leave the last observation live indefinitely.
			if ctx.Err() == nil && recovering {
				m.removeLive(destination)
			}
			return
		}
	}
}

// attemptWatchResult records what unblocked the attempt's frame reader. Only the
// watcher closes stdout; all state changes remain on the sequential worker.
type attemptWatchResult int

const (
	watchReadEnded attemptWatchResult = iota
	watchCanceled
	watchRecoveryTimeout
)

func watchAttempt(ctx context.Context, stdout io.Closer, readDone, firstFrame <-chan struct{}, recoveryTimeout time.Duration) <-chan attemptWatchResult {
	result := make(chan attemptWatchResult, 1)
	go func() {
		waitForEnd := func() attemptWatchResult {
			select {
			case <-ctx.Done():
				_ = stdout.Close()
				return watchCanceled
			case <-readDone:
				return watchReadEnded
			}
		}

		if firstFrame == nil {
			result <- waitForEnd()
			return
		}
		timer := time.NewTimer(recoveryTimeout)
		defer timer.Stop()
		select {
		case <-firstFrame:
			timer.Stop()
			result <- waitForEnd()
		case <-ctx.Done():
			_ = stdout.Close()
			result <- watchCanceled
		case <-readDone:
			result <- watchReadEnded
		case <-timer.C:
			// A confirmation connection which never supplies its initial full
			// snapshot cannot keep old rows alive forever. Closing only this
			// attempt preserves worker serialization; the worker removes the host
			// after ReadFrames returns and still joins the child below.
			_ = stdout.Close()
			result <- watchRecoveryTimeout
		}
	}()
	return result
}

// runAttempt returns true when a previously live host lost transport after at
// least one valid frame and should remain visible through one confirmation
// attempt. A confirmation attempt which fails before its first valid frame
// removes the host here, on the same sequential worker that accepted it.
func (m *Manager) runAttempt(parent context.Context, destination string, recovering bool) bool {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	process, err := m.commands(ctx, destination)
	if err != nil {
		if parent.Err() == nil {
			if recovering {
				m.removeLive(destination)
			}
			m.diagnose(destination, "", DiagnosticCommand)
		}
		return false
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		if parent.Err() == nil {
			if recovering {
				m.removeLive(destination)
			}
			m.diagnose(destination, "", DiagnosticCommand)
		}
		return false
	}
	if err := process.Start(); err != nil {
		_ = stdout.Close()
		if parent.Err() == nil {
			if recovering {
				m.removeLive(destination)
			}
			m.diagnose(destination, "", DiagnosticStart)
		}
		return false
	}

	readDone := make(chan struct{})
	var firstFrame chan struct{}
	if recovering {
		firstFrame = make(chan struct{})
	}
	watchResult := watchAttempt(ctx, stdout, readDone, firstFrame, m.recoveryTimeout)
	sawFrame := false
	readErr := m.readFrames(stdout, m.maxFrameBytes, func(frame Frame) error {
		if err := m.accept(destination, frame); err != nil {
			return err
		}
		if !sawFrame {
			sawFrame = true
			if firstFrame != nil {
				close(firstFrame)
			}
		}
		return nil
	})
	close(readDone)
	watchOutcome := <-watchResult
	cancel()
	if watchOutcome == watchReadEnded {
		_ = stdout.Close()
	}

	loss := classifyLoss(readErr)
	host := m.destinationHost(destination)
	confirmNext := false
	if parent.Err() == nil {
		switch {
		case loss == lossProtocol:
			host, _ = m.removeLive(destination)
		case sawFrame:
			// Even a confirmation connection can later fail. Because it supplied
			// a valid full replacement first, that later failure starts a fresh
			// confirmation cycle rather than proving the prior one permanent.
			confirmNext = true
		case recovering:
			// The one confirmation attempt ended (or timed out) before proving
			// recovery. This is the first observable disconnect edge.
			host, _ = m.removeLive(destination)
		}
	}

	// The visibility decision is made before Wait, but the worker still joins
	// this child before starting another one, so attempts never overlap.
	waitErr := process.Wait()
	if parent.Err() != nil {
		return false
	}
	m.diagnoseAttempt(destination, host, readErr, waitErr, confirmNext, watchOutcome)
	return confirmNext
}

type contactLoss int

const (
	lossTransport contactLoss = iota
	lossProtocol
)

func classifyLoss(readErr error) contactLoss {
	switch {
	case errors.Is(readErr, ErrDuplicateHost), errors.Is(readErr, ErrLocalHostname),
		errors.Is(readErr, ErrHostnameChanged), errors.Is(readErr, ErrSchemaMismatch),
		errors.Is(readErr, ErrInvalidFrame), errors.Is(readErr, ErrFrameTooLarge):
		return lossProtocol
	default:
		// EOF, ordinary read errors, and ErrTruncatedFrame all mean only that
		// transport stopped carrying complete frames. A valid replacement on the
		// next attempt is the evidence that distinguishes a transient gap.
		return lossTransport
	}
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
	// Only this hostname's owning sequential worker can reach this point. An
	// observably identical first frame after a transport gap is recovery, not a
	// state change, so adopting it must not wake aggregate consumers or redraw a
	// TUI. A tombstoned host is always news because accepting it makes it visible.
	prior, existed := m.live[frame.Host]
	_, wasRemoving := m.removing[frame.Host]
	changed := !existed || wasRemoving || !state.ObservablyEqual(prior, frame.Snapshot)
	delete(m.removing, frame.Host)
	m.live[frame.Host] = frame.Snapshot
	if changed {
		m.publishLocked()
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) destinationHost(destination string) string {
	m.mu.RLock()
	host := m.destHosts[destination]
	m.mu.RUnlock()
	return host
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

func (m *Manager) diagnoseAttempt(destination, host string, readErr, waitErr error, reconnecting bool, watchOutcome attemptWatchResult) {
	switch {
	case reconnecting:
		m.diagnose(destination, host, DiagnosticReconnecting)
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
	case watchOutcome == watchRecoveryTimeout:
		m.diagnose(destination, host, DiagnosticDisconnected)
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

// Snapshot returns a detached, point-in-time full replacement map. A present
// host is its latest valid full observation; that observation may survive one
// bounded reconnect-confirmation attempt without producing a subscriber edge.
// Absence means the host never supplied a valid frame or that confirmation
// failed and its routes were invalidated.
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
