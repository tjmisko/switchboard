// Package statustune is the single place every status-color knob and every
// status-decision log line lives, so the chip-color behavior can be tuned and,
// when a color is wrong, diagnosed after the fact.
//
// Two concerns, one package:
//
//   - Tuning collects every threshold and policy switch the daemon's status
//     logic consults (the reconciler's permission decay + delegating promotion,
//     the RPC hook's early red-clear). Defaults live in Default(); a caller can
//     override any field. Nothing about chip color is hard-coded at its decision
//     site — it reads a Tuning field — so changing behavior is a one-line edit
//     here, not a hunt through three packages.
//   - Decision/Log emit the canonical status-decision log line. EVERY color
//     change and every deliberate hold goes through it, recording the full
//     observed state (subagents in flight, the pending tool, how long the chip
//     held) and the rule id that fired. When the user reports "this should not
//     have been orange," grepping `status: pid=<n>` reconstructs exactly what the
//     daemon saw and which rule decided the color — which is the input to the
//     next tuning change. Rule ids map to the case table in
//     docs/status-color-state-model.md §5.
package statustune

import (
	"log"
	"time"
)

// Status string values the tuning policies resolve to. They mirror
// state.Status* (kept as literals to keep this a dependency-free leaf package);
// the renderers map them to colors.
const (
	statusWorking    = "working"
	statusIdle       = "idle"
	statusDelegating = "delegating"
)

// Tuning collects every status-color policy knob. Construct with Default() and
// override fields as needed. Each field documents the error it trades against
// (see docs/status-color-state-model.md §4 for the asymmetric-cost ranking).
type Tuning struct {
	// PermissionDecayTTL bounds how long a red chip stays latched once the
	// transcript check can no longer confirm it is genuinely pending. Per the
	// measurement (§2.1) this backstop fires ~never — the accurate resolution
	// path clears within one tick — so it only governs the unreadable-transcript
	// fail-soft case. Raising it makes a truly-stuck red nag longer; lowering it
	// risks clearing a still-pending red on a transient read failure (missed RED,
	// the worst error).
	PermissionDecayTTL time.Duration

	// TailBytes bounds how much transcript tail every reader consumes. The signals
	// we need live at the end, so a small window stays cheap; too small a window
	// can undercount InFlightTasks on pathologically long turns (see InFlightTasks).
	TailBytes int64

	// EarlyClearApproveByToolName clears a red chip at hook speed when a
	// PostToolUse's tool_name matches the tool the pending prompt was raised for
	// (the approved tool completed), instead of waiting for the transcript to show
	// the turn resumed (the ~26s lag in §2.1). The transcript check remains the
	// fallback, so turning this off only reverts to the slower-but-correct path.
	EarlyClearApproveByToolName bool

	// ResumeExitStatus is the color a red chip exits to when the prompt resolved by
	// the turn resuming (an assistant message — approved). Default working (green):
	// work is happening again, no action needed (P3, case 9). Going direct avoids
	// the red→orange→green bounce (§2.1 secondary latency).
	ResumeExitStatus string

	// InterruptExitStatus is the color a red chip exits to when the prompt resolved
	// by a user interrupt (Esc / declined turn) with no teammates in flight.
	// Default idle (orange): your turn (P3, cases 6/10).
	InterruptExitStatus string

	// DelegatingEnabled renders an idle main thread that still has subagents in
	// flight as green (delegating) rather than orange — "work is happening, no
	// action needed" extended to teammate work (P4, cases 5/14, fixes complaint
	// #2). Off reverts to the old orange-while-teammates-working behavior.
	DelegatingEnabled bool

	// EscWithTeammatesStatus is the color when a turn is interrupted (Esc) OR a
	// prompt is declined while subagents are still in flight (Q3, cases 7/11).
	// Default delegating (green) for consistency with P4 — work is still happening;
	// set to idle (orange) if Esc should mean "I want control now."
	EscWithTeammatesStatus string
}

// Default returns the tuning the daemon ships with: the recommended answers to
// the open questions in docs/status-color-state-model.md §8 (Q1 pure-green
// delegating, Q2 tool-name early clear, Q3 Esc-with-teammates → green).
func Default() Tuning {
	return Tuning{
		PermissionDecayTTL:          30 * time.Second,
		TailBytes:                   128 * 1024,
		EarlyClearApproveByToolName: true,
		ResumeExitStatus:            statusWorking,
		InterruptExitStatus:         statusIdle,
		DelegatingEnabled:           true,
		EscWithTeammatesStatus:      statusDelegating,
	}
}

// Decision is one status-color decision: a transition (From != To) or a
// deliberate hold (From == To). It carries the full observed state the rule saw,
// so a wrong color can be traced to its inputs. Build one and call Log() — every
// status edge in the daemon routes through here.
type Decision struct {
	PID       int           // session pid
	Session   string        // short session id (anchors a chip across restarts)
	From      string        // chip status before the decision
	To        string        // chip status after; == From for a hold
	Rule      string        // stable id, maps to docs/status-color-state-model.md §5
	Reason    string        // short human detail (e.g. "tool-name match: AskUserQuestion")
	Subagents int           // S: subagents in flight at decision time
	Pending   string        // P: the tool the red prompt was raised for ("" when none)
	Age       time.Duration // how long the chip had held From
}

// Log emits the canonical decision line. The `status: pid=` prefix is the stable
// grep anchor (shared with the hook-path edges in rpc); the bracketed tuple is
// the forensic payload. A hold (From == To) is logged too — silence would hide
// "we saw resolution but chose to keep red," which is exactly what a stale-red
// complaint needs to see.
func (d Decision) Log() {
	verb := "->"
	if d.From == d.To {
		verb = "==" // a hold: status unchanged this decision
	}
	log.Printf("status: pid=%d session=%s %s%s%s rule=%s reason=%q [S=%d pending=%q age=%s]",
		d.PID, d.Session, d.From, verb, d.To, d.Rule, d.Reason,
		d.Subagents, d.Pending, d.Age.Round(time.Second))
}
