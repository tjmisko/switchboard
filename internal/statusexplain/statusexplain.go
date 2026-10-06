// Package statusexplain is the content-free record of why a root's published
// status is what it is (#95): which source decided, under which rule, how fresh
// its evidence was, and which candidate readings lost and why.
//
// The types are pure: no I/O and no clock. Every field is an id, an enum or a
// time, so a record can cross RPC without carrying a prompt, a command, a tool
// input or transcript text. Sanitize enforces that shape before a record leaves
// the daemon, and the rejected list is bounded.
package statusexplain

import (
	"fmt"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// Reason is the closed set of rule codes a decision or a rejection carries.
type Reason string

const (
	// The provider graph's own summary decides.
	ReasonGraphAuthority Reason = "graph_authority"
	// The provider graph decides, and the graph that holds it was landed by a
	// hook reduction that owned the transition, bypassing source ranking.
	ReasonHookOwned Reason = "hook_owned"
	// A live herdr reading decides over the provider graph.
	ReasonHerdrOverride Reason = "herdr_override"
	// herdr is live but yields to fresh Codex input or approval attention.
	ReasonHerdrYieldAttention Reason = "herdr_yield_attention"
	// herdr is the only source: the agent has no provider adapter, or a Pi
	// session no hook has bound yet.
	ReasonHerdrOnly Reason = "herdr_only"
	// A bound Pi session's hook evidence has lapsed and live herdr decides.
	ReasonHerdrFallback Reason = "herdr_fallback"
	// A bound Pi session's fresh hook evidence decides.
	ReasonPiHookAuthority Reason = "pi_hook_authority"
	// Publication reads limited over the decision beneath it.
	ReasonUsageLimitOverlay Reason = "usage_limit_overlay"

	// No exact provider session is bound to the root, so nothing observes it.
	ReasonBindingMissing Reason = "binding_missing"
	// A provider session is bound but no observation of it has landed yet.
	ReasonObservationPending Reason = "observation_pending"
	// The evidence that decided has passed its freshness deadline.
	ReasonObservationExpired Reason = "observation_expired"
	// The source in hand cannot classify this root's status.
	ReasonCoverageUnsupported Reason = "coverage_unsupported"
	// The published status was set by a path that records no decision (a
	// legacy hook transition, or state hydrated across a restart). Explain says
	// so rather than attributing it to evidence that did not decide it.
	ReasonUnrecorded Reason = "unrecorded"

	// Rejections at graph admission.
	ReasonSourceOutranked  Reason = "source_outranked"
	ReasonStaleVsFresh     Reason = "stale_vs_fresh"
	ReasonOlderThanCurrent Reason = "older_than_current"
)

var knownReasons = map[Reason]bool{
	ReasonGraphAuthority: true, ReasonHookOwned: true, ReasonHerdrOverride: true,
	ReasonHerdrYieldAttention: true, ReasonHerdrOnly: true, ReasonHerdrFallback: true,
	ReasonPiHookAuthority: true, ReasonUsageLimitOverlay: true, ReasonBindingMissing: true,
	ReasonObservationPending: true, ReasonObservationExpired: true,
	ReasonCoverageUnsupported: true, ReasonUnrecorded: true, ReasonSourceOutranked: true,
	ReasonStaleVsFresh: true, ReasonOlderThanCurrent: true,
}

// Known reports whether r is one of the codes above.
func (r Reason) Known() bool { return knownReasons[r] }

// EvidenceKind classifies a source by what kind of evidence it is.
type EvidenceKind string

const (
	// A provider's own snapshot: app-server, transcript, rollout, session file.
	EvidenceProviderSnapshot EvidenceKind = "provider_snapshot"
	// A provider hook edge.
	EvidenceHook EvidenceKind = "hook"
	// A terminal's reading of the screen (herdr).
	EvidenceTerminal EvidenceKind = "terminal"
	// Last-known state restored across a daemon restart.
	EvidenceRestored EvidenceKind = "restored"
	// Usage-limit evidence, which only publication reads.
	EvidenceUsageLimit EvidenceKind = "usage_limit"
	// No evidence at all.
	EvidenceNone EvidenceKind = "none"
)

var knownEvidence = map[EvidenceKind]bool{
	EvidenceProviderSnapshot: true, EvidenceHook: true, EvidenceTerminal: true,
	EvidenceRestored: true, EvidenceUsageLimit: true, EvidenceNone: true,
}

// Known reports whether k is one of the kinds above.
func (k EvidenceKind) Known() bool { return knownEvidence[k] }

// EvidenceKindOf classifies a graph source. Usage-limit sources are not graph
// sources; their choice names EvidenceUsageLimit itself.
func EvidenceKindOf(source agentgraph.SourceKind) EvidenceKind {
	switch source {
	case agentgraph.SourceCodexAppServer, agentgraph.SourceClaudeTranscript,
		agentgraph.SourceCodexRollout, agentgraph.SourcePiSessionFile:
		return EvidenceProviderSnapshot
	case agentgraph.SourceHook:
		return EvidenceHook
	case agentgraph.SourceHerdr:
		return EvidenceTerminal
	case agentgraph.SourceRestoredLastKnown:
		return EvidenceRestored
	default:
		return EvidenceNone
	}
}

// Root names the process lifetime and provider session a decision belongs to.
type Root struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Provider  string    `json:"provider"`
	SessionID string    `json:"session_id,omitempty"`
}

// Choice is one decided status and the evidence behind it.
type Choice struct {
	Status       string       `json:"status"`
	Source       string       `json:"source,omitempty"`
	EvidenceKind EvidenceKind `json:"evidence_kind"`
	Reason       Reason       `json:"reason"`
	ObservedAt   time.Time    `json:"observed_at,omitzero"`
	// FreshUntil is the evidence's deadline; zero for a live reading (herdr)
	// that holds until the next one.
	FreshUntil time.Time `json:"fresh_until,omitzero"`
	// DecidedAt is when the decision was last made, changed or unchanged.
	DecidedAt time.Time `json:"decided_at,omitzero"`
}

// Candidate is a reading that did not decide, and why.
type Candidate struct {
	Source       string       `json:"source,omitempty"`
	EvidenceKind EvidenceKind `json:"evidence_kind"`
	Status       string       `json:"status"`
	ObservedAt   time.Time    `json:"observed_at,omitzero"`
	FreshUntil   time.Time    `json:"fresh_until,omitzero"`
	RejectReason Reason       `json:"reject_reason"`
}

// Present reports whether c holds a rejection; the zero Candidate is none.
func (c Candidate) Present() bool { return c.RejectReason != "" }

// Projection is the decision a status projection made, kept on the session it
// decided. It is a plain value with no references, so a copied session carries
// a detached copy. StartedAt is the process lifetime it was made for: a record
// read against a different lifetime is not current.
type Projection struct {
	Choice
	StartedAt time.Time
	Rejected  Candidate
}

// MaxRejected bounds a decision's rejected list.
const MaxRejected = 8

// Decision is the explanation of one root's published status. Underlying is
// the decision beneath an overlay (usage limit); Rejected holds the newest
// MaxRejected losing candidates, and RejectedOmitted counts any older ones
// dropped.
type Decision struct {
	Root Root `json:"root"`
	Choice
	Underlying      *Choice     `json:"underlying,omitempty"`
	Rejected        []Candidate `json:"rejected,omitempty"`
	RejectedOmitted int         `json:"rejected_omitted,omitempty"`
}

// AddRejected records a losing candidate. An exact repeat of one already held
// is ignored, so an unchanged decision does not grow its list; past
// MaxRejected the oldest is dropped and counted.
func (d *Decision) AddRejected(c Candidate) {
	if !c.Present() {
		return
	}
	for _, held := range d.Rejected {
		if sameCandidate(held, c) {
			return
		}
	}
	d.Rejected = append(d.Rejected, c)
	if over := len(d.Rejected) - MaxRejected; over > 0 {
		d.Rejected = append([]Candidate(nil), d.Rejected[over:]...)
		d.RejectedOmitted += over
	}
}

func sameCandidate(a, b Candidate) bool {
	return a.Source == b.Source && a.EvidenceKind == b.EvidenceKind && a.Status == b.Status &&
		a.ObservedAt.Equal(b.ObservedAt) && a.FreshUntil.Equal(b.FreshUntil) && a.RejectReason == b.RejectReason
}

// Rejections is a bounded, de-duplicated log of losing candidates kept between
// decisions (the admission layer's).
type Rejections struct {
	list    []Candidate
	omitted int
}

// Add records c with AddRejected's rules.
func (r *Rejections) Add(c Candidate) {
	d := Decision{Rejected: r.list, RejectedOmitted: r.omitted}
	d.AddRejected(c)
	r.list, r.omitted = d.Rejected, d.RejectedOmitted
}

// Reset forgets every candidate.
func (r *Rejections) Reset() { r.list, r.omitted = nil, 0 }

// Into adds the held candidates to d, oldest first, and carries the omitted
// count.
func (r *Rejections) Into(d *Decision) {
	d.RejectedOmitted += r.omitted
	for _, c := range r.list {
		d.AddRejected(c)
	}
}

// Placeholders Sanitize substitutes.
const (
	StatusUnknown = "unknown"
	invalidValue  = "invalid"
)

const (
	maxEnumLen = 32
	maxIDLen   = 128
)

// Sanitize returns d in its wire shape: an empty status reads unknown, and
// any field outside its shape (a reason or evidence kind not in its closed
// set, a status or source that is not a short lowercase code, a session id
// that is not an id) is replaced with "invalid". It is the guarantee that a
// record carries ids, enums and times only, whatever a caller filled in.
func (d Decision) Sanitize() Decision {
	d.Root.Provider = enumValue(d.Root.Provider, "")
	d.Root.SessionID = idValue(d.Root.SessionID)
	d.Choice = d.Choice.sanitize()
	if d.Underlying != nil {
		under := d.Underlying.sanitize()
		d.Underlying = &under
	}
	rejected := d.Rejected
	d.Rejected = nil
	for _, c := range rejected {
		c.Source = enumValue(c.Source, "")
		c.Status = enumValue(c.Status, StatusUnknown)
		c.EvidenceKind = EvidenceKind(knownOr(string(c.EvidenceKind), c.EvidenceKind.Known()))
		c.RejectReason = Reason(knownOr(string(c.RejectReason), c.RejectReason.Known()))
		d.Rejected = append(d.Rejected, c)
	}
	if len(d.Rejected) > MaxRejected {
		d.RejectedOmitted += len(d.Rejected) - MaxRejected
		d.Rejected = d.Rejected[len(d.Rejected)-MaxRejected:]
	}
	return d
}

func (c Choice) sanitize() Choice {
	c.Status = enumValue(c.Status, StatusUnknown)
	c.Source = enumValue(c.Source, "")
	c.EvidenceKind = EvidenceKind(knownOr(string(c.EvidenceKind), c.EvidenceKind.Known()))
	c.Reason = Reason(knownOr(string(c.Reason), c.Reason.Known()))
	return c
}

func knownOr(value string, known bool) string {
	if known {
		return value
	}
	return invalidValue
}

// enumValue keeps a short lowercase code ([a-z_], at most maxEnumLen bytes).
// empty substitutes for "".
func enumValue(value, empty string) string {
	if value == "" {
		return empty
	}
	if len(value) > maxEnumLen {
		return invalidValue
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && r != '_' {
			return invalidValue
		}
	}
	return value
}

// idValue keeps a provider or herdr id: no spaces, no quoting, at most
// maxIDLen bytes. A herdr root may name its socket path, which is allowed.
func idValue(value string) string {
	if len(value) > maxIDLen {
		return invalidValue
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_.:#/", r):
		default:
			return invalidValue
		}
	}
	return value
}

// Text renders d as key=value lines, from the same fields its JSON carries.
func (d Decision) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "root pid=%d started_at=%s provider=%s session_id=%s\n",
		d.Root.PID, textTime(d.Root.StartedAt), textValue(d.Root.Provider), textValue(d.Root.SessionID))
	fmt.Fprintf(&b, "decision %s\n", choiceText(d.Choice))
	if d.Underlying != nil {
		fmt.Fprintf(&b, "underlying %s\n", choiceText(*d.Underlying))
	}
	for _, c := range d.Rejected {
		fmt.Fprintf(&b, "rejected status=%s source=%s evidence_kind=%s observed_at=%s fresh_until=%s reject_reason=%s\n",
			textValue(c.Status), textValue(c.Source), textValue(string(c.EvidenceKind)),
			textTime(c.ObservedAt), textTime(c.FreshUntil), textValue(string(c.RejectReason)))
	}
	if d.RejectedOmitted > 0 {
		fmt.Fprintf(&b, "rejected_omitted=%d\n", d.RejectedOmitted)
	}
	return b.String()
}

func choiceText(c Choice) string {
	return fmt.Sprintf("status=%s reason=%s source=%s evidence_kind=%s observed_at=%s fresh_until=%s decided_at=%s",
		textValue(c.Status), textValue(string(c.Reason)), textValue(c.Source), textValue(string(c.EvidenceKind)),
		textTime(c.ObservedAt), textTime(c.FreshUntil), textTime(c.DecidedAt))
}

func textValue(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// textTime renders t in UTC at the precision JSON carries, so the two forms
// name the same instant.
func textTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339Nano)
}
