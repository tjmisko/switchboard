package statusresolve

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// Target is the tracked agent the candidates must be about.
type Target struct {
	// Root is the agent process lifetime and provider session. Provider is the
	// agent kind; SessionID is "" while no provider session is bound.
	Root statusexplain.Root
	// PaneID is the herdr pane the agent runs in, "" when none.
	PaneID string
	// TerminalID is herdr's stable id for the terminal the agent runs in, ""
	// when herdr has not reported one.
	TerminalID string
}

// Matches reports whether c is about t. A terminal reading must name t's
// agent and terminal: herdr's stable terminal id when both sides carry one,
// else the pane id, which changes when a pane moves workspace. Every other
// kind must name t's agent kind, its bound provider session and its process
// lifetime; nothing matches an unbound root.
func (t Target) Matches(c Candidate) bool {
	id := c.Identity
	if c.Kind == statusexplain.EvidenceTerminal {
		return id.Provider != "" && id.Provider == t.Root.Provider && t.sameTerminal(id)
	}
	return id.Provider != "" && id.Provider == t.Root.Provider &&
		id.SessionID != "" && id.SessionID == t.Root.SessionID &&
		!id.StartedAt.IsZero() && id.StartedAt.Equal(t.Root.StartedAt)
}

func (t Target) sameTerminal(id Identity) bool {
	if t.TerminalID != "" && id.TerminalID != "" {
		return id.TerminalID == t.TerminalID
	}
	return id.PaneID != "" && id.PaneID == t.PaneID
}

// Rank orders the kinds that may select a root's status, highest first. A
// partial hook edge (and any kind not listed) never selects one.
//
//	exact lifecycle event, complete     6  (Pi)
//	coarse terminal reading             5  (herdr; agent and pane must match)
//	provider snapshot                   4  (Claude transcript graph, Codex app-server)
//	exact lifecycle event, edges only   3  (Claude, Codex hooks)
//	correlated transcript evidence      2  (Codex rollout tail, Pi session tail)
//	restored last-known                 1  (state.json; presentation only)
//
// Ranks 2 to 4 are the provider's own evidence. Between two of those that
// both order by event time (Codex), the newer observation wins and rank only
// breaks a tie. Otherwise rank wins, and the newer observation breaks a tie.
func Rank(c Candidate) int {
	switch c.Kind {
	case statusexplain.EvidenceHook:
		if c.CompleteLifecycle {
			return rankCompleteEvent
		}
		return rankEvent
	case statusexplain.EvidenceTerminal:
		return rankTerminal
	case statusexplain.EvidenceProviderSnapshot:
		return rankSnapshot
	case statusexplain.EvidenceTranscript:
		return rankTranscript
	case statusexplain.EvidenceRestored:
		return rankRestored
	default:
		return rankNone
	}
}

const (
	rankNone = iota
	rankRestored
	rankTranscript
	rankEvent
	rankSnapshot
	rankTerminal
	rankCompleteEvent
)

func providerRank(rank int) bool { return rank >= rankTranscript && rank <= rankSnapshot }

// beats reports whether a outranks b for the root's status, and whether rank
// (rather than event time) decided it.
func beats(a, b Candidate) (wins, byRank bool) {
	ra, rb := Rank(a), Rank(b)
	if providerRank(ra) && providerRank(rb) && a.EventTimeOrder && b.EventTimeOrder &&
		!a.ObservedAt.Equal(b.ObservedAt) {
		return a.ObservedAt.After(b.ObservedAt), false
	}
	if ra != rb {
		return ra > rb, true
	}
	return a.ObservedAt.After(b.ObservedAt), false
}

// holdsAttention reports whether c may hold a request for the user open: an
// exact lifecycle event or a provider snapshot that reports one.
func holdsAttention(kind statusexplain.EvidenceKind, attention agentgraph.AttentionState) bool {
	if attention != agentgraph.AttentionApproval && attention != agentgraph.AttentionUserInput {
		return false
	}
	return kind == statusexplain.EvidenceHook || kind == statusexplain.EvidenceProviderSnapshot
}

// claim is an open request for the user: a current candidate's, or the prior
// decision's, held until its own deadline.
type claim struct {
	index  int // into the candidates; -1 for the prior decision
	choice statusexplain.Choice
}

// resolves reports whether r is authorized to resolve the request c holds
// open: r reports no attention, observed after the request, and is either a
// provider snapshot or an exact lifecycle event from the request's own writer
// (the claim's choice names the writer, which for a hook-latched request is
// the hook rather than the snapshot that carried it).
// A terminal reading, a transcript tail, a partial edge and restored
// presentation never resolve one.
func resolves(r Candidate, c claim) bool {
	if r.Status == "" || r.Attention != agentgraph.AttentionNone || !r.ObservedAt.After(c.choice.ObservedAt) {
		return false
	}
	switch r.Kind {
	case statusexplain.EvidenceProviderSnapshot:
		return true
	case statusexplain.EvidenceHook:
		return c.choice.EvidenceKind == statusexplain.EvidenceHook && string(r.Source) == c.choice.Source
	default:
		return false
	}
}

// Resolve decides the root's status from candidates at now. prior is
// Resolve's previous decision for the root (its overlay, if publication added
// one, is ignored); it counts only when it is about the same root.
//
// The kind table. Each kind may establish, and may resolve open attention:
//
//	exact lifecycle event     working, idle, attention   its own writer's only
//	                          (and descendants it carries)
//	provider snapshot         the full graph             yes
//	correlated transcript     working, idle              no
//	coarse terminal reading   working, idle, blocked     no
//	partial hook edge         working descendants        no
//	restored last-known       presentation, old deadline no
//
// The rules, in order:
//
//  1. A candidate that is not about the target, or not fresh at now, cannot
//     select; one that cannot classify the root contributes nothing; one a
//     newer observation superseded may only hold its own request open.
//  2. The best remaining candidate by Rank is the base.
//  3. Open provider attention (a current exact event's or snapshot's, or the
//     prior decision's until its deadline) holds the root at permission
//     against any base not itself permission, until a candidate authorized to
//     resolve that request reports it resolved.
//  4. With no base and no open attention, the prior decision holds until its
//     own deadline; a live reading with no deadline does not hold.
//  5. Then restored presentation may decide, until its persisted deadline.
//  6. An idle result stays delegating while the newest fresh snapshot,
//     partial edge or exact event reports working descendants.
func Resolve(target Target, candidates []Candidate, prior statusexplain.Decision, now time.Time) statusexplain.Decision {
	d, _ := ResolveIndex(target, candidates, prior, now)
	return d
}

// ResolveIndex is Resolve, also returning the index of the candidate the
// decision rests on: the one that selected the status, the one holding the
// request open, or the one reporting live descendants; -1 when the prior
// decision holds or nothing decides.
func ResolveIndex(target Target, candidates []Candidate, prior statusexplain.Decision, now time.Time) (statusexplain.Decision, int) {
	d := statusexplain.Decision{Root: target.Root}
	reasons := make([]statusexplain.Reason, len(candidates))
	admitted := make([]bool, len(candidates))
	for i, c := range candidates {
		switch {
		case !target.Matches(c):
			reasons[i] = statusexplain.ReasonIdentityMismatch
		case !c.Fresh(now):
			reasons[i] = statusexplain.ReasonObservationExpired
		default:
			admitted[i] = true
			reasons[i] = statusexplain.ReasonCoverageUnsupported
		}
	}

	base, restored := -1, -1
	for i, c := range candidates {
		if admitted[i] && c.Superseded {
			reasons[i] = statusexplain.ReasonOlderThanCurrent
		}
		if !admitted[i] || c.Status == "" || c.Superseded {
			continue
		}
		switch Rank(c) {
		case rankNone:
		case rankRestored:
			if restored < 0 || c.ObservedAt.After(candidates[restored].ObservedAt) {
				restored = i
			}
		default:
			if base < 0 {
				base = i
				continue
			}
			if wins, _ := beats(c, candidates[base]); wins {
				base = i
			}
		}
	}
	for i, c := range candidates {
		if !admitted[i] || c.Status == "" || c.Superseded || Rank(c) == rankNone || i == base {
			continue
		}
		reasons[i] = statusexplain.ReasonOlderThanCurrent
		if base < 0 {
			continue // an older restored candidate
		}
		if _, byRank := beats(candidates[base], c); byRank {
			reasons[i] = statusexplain.ReasonSourceOutranked
		}
	}

	held, priorChoice := priorFor(target, prior)
	var choice statusexplain.Choice
	chosen := -1
	switch open, ok := openAttention(candidates, admitted, held, priorChoice, now); {
	case ok && (base < 0 || candidates[base].Status != agentgraph.LegacyPermission):
		choice = open.choice
		choice.Status = agentgraph.LegacyPermission
		choice.Reason = statusexplain.ReasonAttentionHeld
		chosen = open.index
		if base >= 0 {
			reasons[base] = statusexplain.ReasonAttentionHeld
		}
	case base >= 0:
		chosen = base
		choice = choiceOf(candidates[base], authority(candidates[base]))
		if b := candidates[base]; holdsAttention(b.Kind, b.Attention) {
			// The request is its writer's: a later decision holding it as prior
			// must know who may resolve it.
			w := b.writer()
			choice.EvidenceKind, choice.Source = w.Kind, string(w.Source)
		}
	case held && priorChoice.Status != "" && !priorChoice.FreshUntil.IsZero() && now.Before(priorChoice.FreshUntil):
		choice = priorChoice
		choice.Reason = statusexplain.ReasonPriorHeld
	case restored >= 0:
		chosen = restored
		choice = choiceOf(candidates[restored], statusexplain.ReasonLastKnown)
	default:
		choice = statusexplain.Choice{EvidenceKind: statusexplain.EvidenceNone, Reason: unknownReason(target, reasons, admitted)}
	}
	if restored >= 0 && chosen != restored {
		reasons[restored] = statusexplain.ReasonSourceOutranked
	}

	if choice.Status == agentgraph.LegacyIdle && choice.EvidenceKind != statusexplain.EvidenceRestored {
		if live := newestDescendants(candidates, admitted); live >= 0 && candidates[live].WorkingDescendants > 0 {
			if chosen >= 0 && chosen != live {
				reasons[chosen] = statusexplain.ReasonDescendantsLive
			}
			chosen = live
			choice = choiceOf(candidates[live], statusexplain.ReasonDescendantsLive)
			choice.Status = agentgraph.LegacyDelegating
		}
	}

	choice.DecidedAt = now
	d.Choice = choice
	for i, c := range candidates {
		if i == chosen {
			continue
		}
		d.AddRejected(statusexplain.Candidate{
			Source: string(c.Source), EvidenceKind: c.Kind, Status: c.Status,
			ObservedAt: c.ObservedAt, FreshUntil: c.FreshUntil, RejectReason: reasons[i],
		})
	}
	return d, chosen
}

// priorFor returns prior's decision beneath any overlay, and whether it is
// about target's root.
func priorFor(target Target, prior statusexplain.Decision) (bool, statusexplain.Choice) {
	choice := prior.Choice
	if prior.Underlying != nil {
		choice = *prior.Underlying
	}
	r, t := prior.Root, target.Root
	same := r.PID == t.PID && r.StartedAt.Equal(t.StartedAt) && r.Provider == t.Provider && r.SessionID == t.SessionID
	return same && !t.StartedAt.IsZero(), choice
}

// openAttention returns the newest request for the user that nothing
// authorized has resolved: from a fresh exact event or snapshot, or from the
// prior decision while its deadline lasts.
func openAttention(candidates []Candidate, admitted []bool, held bool, prior statusexplain.Choice, now time.Time) (claim, bool) {
	var claims []claim
	for i, c := range candidates {
		if admitted[i] && holdsAttention(c.Kind, c.Attention) {
			choice := choiceOf(c, statusexplain.ReasonAttentionHeld)
			w := c.writer()
			choice.EvidenceKind, choice.Source = w.Kind, string(w.Source)
			claims = append(claims, claim{index: i, choice: choice})
		}
	}
	if held && prior.Status == agentgraph.LegacyPermission && !prior.FreshUntil.IsZero() && now.Before(prior.FreshUntil) &&
		(prior.EvidenceKind == statusexplain.EvidenceHook || prior.EvidenceKind == statusexplain.EvidenceProviderSnapshot) {
		claims = append(claims, claim{index: -1, choice: prior})
	}
	open, found := claim{}, false
	for _, c := range claims {
		resolved := false
		for i, r := range candidates {
			if admitted[i] && resolves(r, c) {
				resolved = true
				break
			}
		}
		if resolved {
			continue
		}
		if !found || c.choice.ObservedAt.After(open.choice.ObservedAt) {
			open, found = c, true
		}
	}
	return open, found
}

// newestDescendants returns the newest fresh candidate that may report the
// root's descendants (a provider snapshot, a partial hook edge, an exact event
// or the Codex rollout tail's correction), or -1.
func newestDescendants(candidates []Candidate, admitted []bool) int {
	newest := -1
	for i, c := range candidates {
		if !admitted[i] || c.Superseded || !reportsDescendants(c) {
			continue
		}
		if newest < 0 || c.ObservedAt.After(candidates[newest].ObservedAt) ||
			(c.ObservedAt.Equal(candidates[newest].ObservedAt) && c.WorkingDescendants > candidates[newest].WorkingDescendants) {
			newest = i
		}
	}
	return newest
}

func reportsDescendants(c Candidate) bool {
	switch c.Kind {
	case statusexplain.EvidenceProviderSnapshot, statusexplain.EvidenceHookEdge, statusexplain.EvidenceHook:
		return true
	case statusexplain.EvidenceTranscript:
		return c.Source == agentgraph.SourceCodexRollout
	default:
		return false
	}
}

func authority(c Candidate) statusexplain.Reason {
	switch c.Kind {
	case statusexplain.EvidenceHook:
		return statusexplain.ReasonEventAuthority
	case statusexplain.EvidenceTerminal:
		return statusexplain.ReasonTerminalAuthority
	case statusexplain.EvidenceTranscript:
		return statusexplain.ReasonTranscriptAuthority
	default:
		return statusexplain.ReasonGraphAuthority
	}
}

func choiceOf(c Candidate, reason statusexplain.Reason) statusexplain.Choice {
	return statusexplain.Choice{
		Status: c.Status, Source: string(c.Source), EvidenceKind: c.Kind, Reason: reason,
		ObservedAt: c.ObservedAt, FreshUntil: c.FreshUntil,
	}
}

// unknownReason says why nothing decided: a fresh candidate that could not
// classify the root, else expired evidence, else evidence about another
// agent, else nothing yet (or nothing bound to look for).
func unknownReason(target Target, reasons []statusexplain.Reason, admitted []bool) statusexplain.Reason {
	has := func(want statusexplain.Reason, fresh bool) bool {
		for i, r := range reasons {
			if r == want && admitted[i] == fresh {
				return true
			}
		}
		return false
	}
	switch {
	case has(statusexplain.ReasonCoverageUnsupported, true):
		return statusexplain.ReasonCoverageUnsupported
	case has(statusexplain.ReasonObservationExpired, false):
		return statusexplain.ReasonObservationExpired
	case has(statusexplain.ReasonIdentityMismatch, false):
		return statusexplain.ReasonIdentityMismatch
	case target.Root.SessionID == "" && target.PaneID == "":
		return statusexplain.ReasonBindingMissing
	default:
		return statusexplain.ReasonObservationPending
	}
}
