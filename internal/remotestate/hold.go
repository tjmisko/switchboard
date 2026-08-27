package remotestate

import (
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// The hysteresis this file implements exists because an SSH stream's health and
// a remote session's existence are different facts, and the original design
// conflated them: any stream loss removed every row that stream carried, so a
// two-second reconnect blanked and restored a whole host's chips on the bar.
//
// The replacement separates the two questions:
//
//	Did the peer TELL us it was going away?   -> drop now (Closeout).
//	Did we merely stop being able to hear it? -> hold its last observation.
//
// A held host passes through three phases, each with a distinct justification:
//
//	quiet (0 .. QuietFor)          rows unchanged, no publish at all. Nothing
//	                               about the host could plausibly have changed
//	                               yet, so saying anything would be noise —
//	                               including saying "stale", which would just
//	                               trade a disappearing chip for a blinking one.
//	stale (QuietFor .. HoldFor)    rows held but marked Stale with the local
//	                               last-contact instant. Contact is properly
//	                               lost; the values are the last true ones and
//	                               the renderer says so.
//	dropped (HoldFor)              the host is gone. Held rows are not evidence
//	                               of anything after long enough, and a bar full
//	                               of frozen chips is worse than an empty one.
//
// The phases are driven by ONE timer per host, rearmed at each transition and
// fenced by the host's epoch, so a reconnect racing an expiry cannot resurrect
// a dropped host or drop a live one.
const (
	// DefaultQuietFor is the invisible hold: how long a lost stream produces no
	// observable change at all.
	//
	// It is sized against the reconnect path it is meant to cover, not guessed:
	// a worker waits DefaultRetryDelay (2s), then pays one SSH connect. Six
	// seconds clears that with room for a slow handshake, and stays well under
	// the interval in which a session's status could realistically change and
	// matter.
	DefaultQuietFor = 6 * time.Second

	// DefaultHoldFor is the drop deadline: how long a host's last observation
	// may stand with no contact.
	//
	// Long enough to ride out a wifi blip, a remote daemon restart, or a route
	// flap; short enough that a machine which really went away does not linger
	// on the bar past the point of belief. It is emphatically NOT sized to
	// cover a laptop suspend — there is no honest number for that, and the
	// stale marker is what protects a user who returns to a held row.
	DefaultHoldFor = 45 * time.Second

	// MaxHoldFor bounds the configurable hold. A hold longer than this is not
	// hysteresis, it is a cache of things that no longer exist.
	MaxHoldFor = 10 * time.Minute

	// silenceMultiple sets the no-frames-heard deadline for a peer that
	// advertises a keepalive: rows go stale after this many missed periods
	// while the stream is still nominally up.
	//
	// Three, not two: one lost frame must not be enough. The advertisement is a
	// floor ("at least this often"), a busy remote's write can be delayed
	// behind a large snapshot, and the penalty for being early is a wrongly
	// dimmed chip on a perfectly healthy host.
	silenceMultiple = 3
)

// hostState is one remote host's held observation plus the client-side contact
// facts that decide how much of it to believe.
type hostState struct {
	// owner is the destination whose worker claimed this hostname. Claims are
	// sticky for the Manager's lifetime, so this never changes once set.
	owner    string
	snapshot state.Snapshot
	// keepalive is the period the peer advertised, or 0 for a peer that made no
	// promise — in which case no silence deadline applies and transport-level
	// detection is the only signal, exactly as before this change.
	keepalive time.Duration

	// lastContact is the CLIENT's clock at the last accepted frame.
	lastContact time.Time
	// connected distinguishes "the stream is up and quiet" from "the stream is
	// gone and we are holding". Both can be stale; only the second is on a
	// countdown to removal.
	connected bool
	// lostAt is when contact was lost; the origin for the quiet and hold
	// deadlines. Zero while connected.
	lostAt time.Time
	// stale is the published phase, not a derived predicate. Keeping it as
	// stored state is what makes a phase change an EDGE the manager can publish
	// on, rather than something a reader might or might not notice depending on
	// when it happened to look.
	stale bool

	// epoch fences timer callbacks. Every event that changes what the deadlines
	// mean bumps it, so a callback that was already in flight when a frame
	// arrived recognizes itself as obsolete and does nothing.
	epoch uint64
	timer Timer
}

// stamp writes the staleness verdict onto the detached copy a consumer sees.
//
// The marker is stamped HERE, on the outgoing copy, rather than published as a
// separate host-level liveness map, for one reason: a consumer that had to read
// rows and liveness in two calls could tear between them and render a host as
// both live and gone. One read, one answer.
func (v hostView) stamp(snapshot state.Snapshot) state.Snapshot {
	if !v.stale {
		return snapshot
	}
	lastContact := v.lastContact
	for i := range snapshot.Sessions {
		snapshot.Sessions[i].Stale = true
		// Each row gets its own pointer: the copy is handed to arbitrary
		// consumers and one shared *time.Time would let any of them alter what
		// every other row reports.
		stamp := lastContact
		snapshot.Sessions[i].LastContact = &stamp
	}
	return snapshot
}

// silenceDeadline is when a still-connected host should be considered out of
// touch, or zero when its peer advertises no keepalive and the question cannot
// be answered from the application stream.
func (h *hostState) silenceDeadline() time.Time {
	if h.keepalive <= 0 {
		return time.Time{}
	}
	return h.lastContact.Add(silenceMultiple * h.keepalive)
}

// holdPhase is what a host's deadlines say it should be at instant now.
type holdPhase int

const (
	phaseFresh holdPhase = iota
	phaseStale
	phaseDrop
)

// phaseAt evaluates the state machine: the phase the host belongs in now, and
// how long until the next transition (zero when there is no further deadline).
//
// It is a pure function of stored state and now, with no side effects, so the
// timer callback, the frame path, and the tests all ask the same question and
// cannot answer it differently.
func (m *Manager) phaseAt(h *hostState, now time.Time) (holdPhase, time.Duration) {
	if h.connected {
		deadline := h.silenceDeadline()
		if deadline.IsZero() {
			return phaseFresh, 0
		}
		if !now.Before(deadline) {
			// A silent-but-connected host is marked, never dropped. SSH's own
			// keepalive is the authority on whether the link is dead, and
			// removing rows on application silence alone would mean a peer whose
			// keepalive we mis-parsed could delete a healthy host.
			return phaseStale, 0
		}
		return phaseFresh, deadline.Sub(now)
	}
	dropAt := h.lostAt.Add(m.holdFor)
	if !now.Before(dropAt) {
		return phaseDrop, 0
	}
	staleAt := h.lostAt.Add(m.quietFor)
	if !now.Before(staleAt) {
		return phaseStale, dropAt.Sub(now)
	}
	return phaseFresh, staleAt.Sub(now)
}

// armLocked re-evaluates a host and schedules its next transition. It returns
// true when the host must be dropped, which the caller performs after releasing
// the lock (removal runs a route-invalidation callback that may read the
// Manager back).
//
// Callers must hold m.mu.
func (m *Manager) armLocked(host string, h *hostState) (phaseChanged bool, drop bool) {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.epoch++
	epoch := h.epoch

	phase, next := m.phaseAt(h, m.clock.Now())
	if phase == phaseDrop {
		return false, true
	}
	stale := phase == phaseStale
	phaseChanged = stale != h.stale
	h.stale = stale
	if next > 0 {
		h.timer = m.clock.AfterFunc(next, func() { m.onDeadline(host, epoch) })
	}
	return phaseChanged, false
}

// onDeadline advances one host at its scheduled transition.
func (m *Manager) onDeadline(host string, epoch uint64) {
	m.mu.Lock()
	h := m.hosts[host]
	if h == nil || h.epoch != epoch {
		// A frame, a disconnect, or a shutdown moved this host after the timer
		// was armed. Whatever did so armed its own successor.
		m.mu.Unlock()
		return
	}
	if m.stopped {
		m.mu.Unlock()
		return
	}
	phaseChanged, drop := m.armLocked(host, h)
	if drop {
		// armLocked bumped the epoch; the removal must be fenced by the CURRENT
		// one, not the one this callback was armed with.
		owner, current := h.owner, h.epoch
		m.mu.Unlock()
		if m.dropHost(host, owner, current) {
			m.diagnose(owner, host, DiagnosticHoldExpired)
		}
		return
	}
	if phaseChanged {
		m.publishLocked()
	}
	m.mu.Unlock()
}
