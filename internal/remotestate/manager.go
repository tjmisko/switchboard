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
	// DiagnosticTruncated separates a link cut mid-frame from a peer that sent
	// something unreadable. Both end the stream; only the first holds rows.
	DiagnosticTruncated = "truncated"
	// DiagnosticHeld replaces DiagnosticDisconnected when the host's last
	// observation was retained instead of removed.
	DiagnosticHeld = "held"
	// DiagnosticHoldExpired is a host removed because contact never came back
	// within HoldFor.
	DiagnosticHoldExpired = "hold_expired"
	// DiagnosticCloseout is the peer's own statement that it was going away, so
	// its rows were dropped at once rather than held.
	DiagnosticCloseout = "closeout"
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
	// Reason accompanies DiagnosticCloseout. It is peer-supplied, so it is
	// validated to a bare lowercase token before it can reach a log line; empty
	// means the peer named no reason.
	Reason string
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
	//
	// With a hold configured it fires at the END of the hold, not at the
	// disconnect. That is deliberate: a held row stays navigable because the
	// thing a click actually focuses — the LOCAL terminal pane displaying that
	// SSH session — has not moved. Losing the state stream says nothing about
	// where the user's window is, and the moment contact is lost is exactly the
	// moment they are most likely to want to look at it.
	OnHostRemoved func(string)

	// HoldFor is how long a host's last observation stands after contact is
	// lost without a closeout. ZERO DISABLES THE HOLD, restoring removal at the
	// disconnect; callers that want hysteresis pass DefaultHoldFor explicitly.
	// The default is off rather than on so that no caller acquires held rows —
	// which outlive their evidence — without having asked for them.
	HoldFor time.Duration
	// QuietFor is how much of HoldFor passes with no observable change at all
	// before held rows are marked stale. Ignored when HoldFor is zero. A value
	// at or above HoldFor simply means rows are dropped without ever being
	// shown as stale.
	QuietFor time.Duration
	// Clock is the deadline time base. Nil selects the system clock.
	Clock Clock
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

	holdFor  time.Duration
	quietFor time.Duration
	clock    Clock

	mu    sync.RWMutex
	hosts map[string]*hostState
	// removing tombstones a host for the duration of its route-invalidation
	// callback, so Snapshot and any concurrent publication already omit it
	// before routes are torn down.
	removing   map[string]struct{}
	hostOwners map[string]string
	destHosts  map[string]string
	// closeouts holds the reason a destination's peer gave for going away,
	// between the read loop that saw it and the disconnect that acts on it.
	closeouts   map[string]string
	subscribers map[chan map[string]state.Snapshot]struct{}
	ran         bool
	// stopped freezes the hold machinery once Run returns: a deadline that
	// fires during shutdown must not call OnHostRemoved into a torn-down view.
	stopped bool
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
	if config.HoldFor < 0 || config.HoldFor > MaxHoldFor {
		return nil, fmt.Errorf("hold must be between 0 and %s", MaxHoldFor)
	}
	if config.QuietFor < 0 {
		return nil, fmt.Errorf("quiet window must not be negative")
	}
	clock := config.Clock
	if clock == nil {
		clock = systemClock{}
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
		holdFor:       config.HoldFor,
		quietFor:      config.QuietFor,
		clock:         clock,
		hosts:         make(map[string]*hostState),
		removing:      make(map[string]struct{}),
		hostOwners:    make(map[string]string),
		destHosts:     make(map[string]string),
		closeouts:     make(map[string]string),
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

// newSSHCommand builds the fixed argument vector. The keepalive is deliberately
// AGGRESSIVE — a dead link is declared in roughly 15s rather than 30s — which is
// only safe because a declared-dead link no longer blanks the bar: the hold
// absorbs the disconnect while SSH gets on with finding out the truth. Fast
// detection and hysteresis are complements, not alternatives.
func newSSHCommand(ctx context.Context, destination string) (Process, error) {
	cmd := exec.CommandContext(ctx, "ssh",
		"-n", "-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
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
	m.stopHolds()
	return nil
}

// stopHolds disarms every pending deadline once the workers are done. Held rows
// are left in place rather than dropped: nothing reads them after shutdown, and
// firing route invalidation into a view that is itself being torn down would be
// a shutdown-ordering hazard for no benefit.
func (m *Manager) stopHolds() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	for _, h := range m.hosts {
		if h.timer != nil {
			h.timer.Stop()
			h.timer = nil
		}
	}
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
	outcome := m.endContact(destination, classifyLoss(readErr))
	// Contact is resolved before Wait: an SSH child can be slow to reap, but its
	// dead stream must never leave rows that claim to be live. The worker still
	// waits below before returning to the retry loop, so child processes never
	// overlap.
	waitErr := process.Wait()
	if parent.Err() != nil {
		return
	}
	m.diagnoseAttempt(destination, outcome, readErr, waitErr)
}

// contactLoss classifies why a stream ended. It is the whole decision: what the
// client may still believe about a host depends entirely on which of these
// happened, and conflating them is the bug this change exists to fix.
type contactLoss int

const (
	// lossTransport: we stopped being able to hear the peer. It says nothing
	// about the sessions on that machine, so the last observation may stand.
	lossTransport contactLoss = iota
	// lossProtocol: the peer is reachable but sent something this client cannot
	// use. Holding rows we can never refresh would be a lie with no expiry other
	// than the timer, and the condition (a schema skew, a hostname flip, a
	// duplicate claim) usually survives the reconnect. Drop now.
	lossProtocol
	// lossCloseout: the peer said it was going away. Drop now.
	lossCloseout
)

func classifyLoss(readErr error) contactLoss {
	switch {
	case errors.Is(readErr, ErrCloseout):
		return lossCloseout
	case errors.Is(readErr, ErrDuplicateHost), errors.Is(readErr, ErrLocalHostname),
		errors.Is(readErr, ErrHostnameChanged), errors.Is(readErr, ErrSchemaMismatch),
		errors.Is(readErr, ErrInvalidFrame), errors.Is(readErr, ErrFrameTooLarge):
		return lossProtocol
	default:
		// EOF, a read error, and a truncated final frame are all the transport
		// failing. So is the default: an unclassified error is far likelier to be
		// a broken pipe than a protocol violation, and the safe reading of an
		// unknown failure is "we lost the link", not "delete the host".
		return lossTransport
	}
}

// accept validates one frame and folds it into the live map. It runs on the
// owning destination's read goroutine, so a single host's frames are always
// applied in order.
func (m *Manager) accept(destination string, frame Frame) error {
	canonical, err := CanonicalHostname(frame.Host)
	if err != nil || canonical != frame.Host {
		return ErrInvalidFrame
	}
	if err := validateFrameBody(frame); err != nil {
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
	if frame.Closeout != nil {
		// Record the reason for the disconnect diagnostic and end the read loop.
		// The peer may keep talking; nothing after a closeout is read, so it
		// cannot take back what it just said.
		//
		// Checked after the claim GUARDS but before the claim itself: a stranger
		// must not be able to close out a hostname another destination owns, and
		// a peer whose only frame was a goodbye should not leave a sticky
		// ownership claim behind for a host it never described.
		m.closeouts[destination] = frame.Closeout.Reason
		m.mu.Unlock()
		return ErrCloseout
	}
	if claimedHost == "" {
		m.destHosts[destination] = frame.Host
		m.hostOwners[frame.Host] = destination
	}

	// Only this hostname's owning sequential worker can reach this point. A
	// reconnect adopts a fresh full frame and makes the host visible again.
	_, wasRemoving := m.removing[frame.Host]
	delete(m.removing, frame.Host)
	h := m.hosts[frame.Host]
	// A frame is news when a consumer could tell the difference: the host is
	// new, it was tombstoned or marked stale, or the state itself moved. A
	// keepalive re-send of unchanged state on a healthy stream is none of those,
	// and republishing it would push an identical aggregate through every
	// subscriber and every waybar slot on a timer.
	changed := h == nil || wasRemoving || h.stale
	if h == nil {
		h = &hostState{owner: destination}
		m.hosts[frame.Host] = h
	} else if !changed {
		changed = !state.ObservablyEqual(h.snapshot, *frame.Snapshot)
	}
	h.snapshot = *frame.Snapshot
	h.keepalive = time.Duration(frame.KeepaliveSeconds) * time.Second
	h.lastContact = m.clock.Now()
	h.connected = true
	h.lostAt = time.Time{}
	phaseChanged, _ := m.armLocked(frame.Host, h)
	if changed || phaseChanged {
		m.publishLocked()
	}
	m.mu.Unlock()
	return nil
}

// contactOutcome is what one lost stream did to the client's belief about its
// host: which host it was, whether the last observation was retained, and the
// reason the peer gave if it closed out deliberately.
type contactOutcome struct {
	Host   string
	Held   bool
	Reason string
}

// endContact resolves a destination's lost stream.
func (m *Manager) endContact(destination string, loss contactLoss) contactOutcome {
	m.mu.Lock()
	host := m.destHosts[destination]
	outcome := contactOutcome{Host: host, Reason: m.closeouts[destination]}
	delete(m.closeouts, destination)
	h := m.hosts[host]
	if host == "" || h == nil || h.owner != destination {
		m.mu.Unlock()
		return outcome
	}
	if loss != lossTransport || m.holdFor == 0 || m.stopped {
		epoch := h.epoch
		m.mu.Unlock()
		m.dropHost(host, destination, epoch)
		return outcome
	}
	if !h.connected {
		// Already holding. A retry that fails before it reads a frame must not
		// restart the countdown, or a flapping destination could hold rows
		// forever without ever refreshing them.
		m.mu.Unlock()
		outcome.Held = true
		return outcome
	}
	h.connected = false
	h.lostAt = m.clock.Now()
	phaseChanged, drop := m.armLocked(host, h)
	epoch := h.epoch
	if drop {
		// Reachable when HoldFor is small enough that the deadline is already
		// behind us; take the same path an expiry would.
		m.mu.Unlock()
		m.dropHost(host, destination, epoch)
		return outcome
	}
	if phaseChanged {
		m.publishLocked()
	}
	m.mu.Unlock()
	outcome.Held = true
	return outcome
}

// dropHost removes a host and invalidates its routes. epoch fences the removal
// against a reconnect: every event that changes a host's meaning bumps it, so a
// decision taken before that event cannot be applied after it.
func (m *Manager) dropHost(host, owner string, epoch uint64) bool {
	m.mu.Lock()
	h := m.hosts[host]
	if h == nil || h.owner != owner || h.epoch != epoch {
		m.mu.Unlock()
		return false
	}
	if _, removing := m.removing[host]; removing {
		m.mu.Unlock()
		return false
	}
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
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
	defer m.mu.Unlock()
	if h = m.hosts[host]; h == nil || h.epoch != epoch {
		// A frame arrived while routes were being invalidated and re-adopted the
		// host. Its own publish may have raced AHEAD of the invalidation above,
		// leaving the live-route projection holding a map it has already acted
		// on; republish so it re-reads the current one and restores the routes
		// the callback just dropped.
		delete(m.removing, host)
		m.publishLocked()
		return false
	}
	delete(m.hosts, host)
	delete(m.removing, host)
	m.publishLocked()
	return true
}

func (m *Manager) diagnoseAttempt(destination string, outcome contactOutcome, readErr, waitErr error) {
	host := outcome.Host
	switch {
	case errors.Is(readErr, ErrCloseout):
		m.emitDiagnostic(Diagnostic{Destination: destination, Host: host,
			Category: DiagnosticCloseout, Reason: outcome.Reason})
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
	case errors.Is(readErr, ErrTruncatedFrame):
		m.diagnose(destination, host, m.lossCategory(DiagnosticTruncated, outcome.Held))
	case readErr != nil && !errors.Is(readErr, io.EOF):
		m.diagnose(destination, host, m.lossCategory(DiagnosticRead, outcome.Held))
	case waitErr != nil:
		m.diagnose(destination, host, m.lossCategory(DiagnosticDisconnected, outcome.Held))
	default:
		m.diagnose(destination, host, m.lossCategory(DiagnosticDisconnected, outcome.Held))
	}
}

// lossCategory reports the held category in place of the transport-failure one
// when rows survived, so a log reader can tell "the bar just emptied" from "the
// bar is showing you what it last knew".
func (m *Manager) lossCategory(category string, held bool) string {
	if held {
		return DiagnosticHeld
	}
	return category
}

func (m *Manager) diagnose(destination, host, category string) {
	m.emitDiagnostic(Diagnostic{Destination: destination, Host: host, Category: category})
}

func (m *Manager) emitDiagnostic(diagnostic Diagnostic) {
	if m.onDiagnostic != nil {
		m.onDiagnostic(diagnostic)
	}
}

// Snapshot returns a detached, point-in-time full replacement map.
//
// Absence of a hostname means the client is no longer willing to say anything
// about it: either its stream never connected, or contact was lost and either
// closed out by the peer or held past HoldFor. PRESENCE no longer means the
// stream is live — a present host whose rows carry Stale is one whose last
// observation is being held. That distinction is carried on the rows rather
// than in a second map on purpose: one read gives a consumer both the state and
// how much to trust it, with no window in which the two can disagree.
func (m *Manager) Snapshot() map[string]state.Snapshot {
	m.mu.RLock()
	views := m.viewLocked()
	m.mu.RUnlock()
	// Detach outside the lock: the deep copy is a JSON round trip per host, and
	// a reader must not hold the mutex through it. viewLocked already captured
	// each host's staleness verdict alongside its rows, so the pair cannot tear.
	return detachViews(views)
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
	// Still under the write lock, as it was before the hold existed: fanout has
	// to stay ordered with the mutation that caused it, and a subscriber left
	// holding an older complete map would be stranded there until the next
	// change — which, on a final disconnect, never comes.
	view := detachViews(m.viewLocked())
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

// hostView pairs one host's stored rows with the staleness verdict that must be
// published with them. Capturing both under one lock acquisition is what stops
// a consumer from seeing rows from one instant and a verdict from another.
type hostView struct {
	snapshot    state.Snapshot
	stale       bool
	lastContact time.Time
}

// viewLocked collects the currently visible hosts. It is deliberately cheap —
// struct copies that still share their Sessions backing array — so the
// expensive detach can happen after the lock is released. Sharing is safe
// because a stored snapshot is only ever REPLACED, never mutated in place.
func (m *Manager) viewLocked() map[string]hostView {
	views := make(map[string]hostView, len(m.hosts))
	for host, h := range m.hosts {
		if _, removing := m.removing[host]; removing {
			continue
		}
		views[host] = hostView{snapshot: h.snapshot, stale: h.stale, lastContact: h.lastContact}
	}
	return views
}

// detachViews builds the map handed to callers: one deep copy per host, stamped
// with that host's staleness verdict.
//
// The stamp is applied AFTER the copy, never to the stored snapshot. The stored
// value is the peer's own last word and must stay that way — a held row that
// reconnects has to compare equal to the frame that refreshes it, and a stored
// Stale flag would make every reconnect look like news.
func detachViews(views map[string]hostView) map[string]state.Snapshot {
	out := make(map[string]state.Snapshot, len(views))
	for host, view := range views {
		out[host] = view.stamp(detachSnapshot(view.snapshot))
	}
	return out
}

func detachSnapshot(snapshot state.Snapshot) state.Snapshot {
	// Remote snapshots arrived through JSON, so a wire round trip is the
	// smallest reliable deep copy of nested provider graph slices/pointers.
	// All state fields are JSON-representable; retain the original only if a
	// future field violates that invariant, rather than dropping a live host.
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return snapshot
	}
	var detached state.Snapshot
	if err := json.Unmarshal(encoded, &detached); err != nil {
		return snapshot
	}
	return detached
}
