package statusresolve

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// Candidate is one source's reading of a root, shaped by its builder into what
// that kind of evidence may establish. Every field is an id, an enum, a count
// or a time: a candidate carries no content.
type Candidate struct {
	// Kind is what the evidence is, and so what it may do (see the kind table
	// on Resolve). Its builder sets it; the source name never does.
	Kind   statusexplain.EvidenceKind
	Source agentgraph.SourceKind
	// Identity is what the evidence claims to be about. Resolve compares it
	// with the tracked agent before anything else.
	Identity Identity
	// Status is the legacy status the evidence establishes for the root
	// (working, idle, permission, delegating), "" when it cannot say.
	Status string
	// Attention is provider attention the evidence reports (approval or user
	// input, on the root or a live descendant). Only an exact lifecycle event or
	// a provider snapshot carries it; other kinds report none.
	Attention agentgraph.AttentionState
	// WorkingDescendants counts positively live descendants doing work. A
	// provider snapshot, a partial hook edge, or an exact event landed on a
	// composed graph reports them; other kinds report none.
	WorkingDescendants int
	// CompleteLifecycle marks an exact lifecycle event whose writer reports
	// both edges of every state it asserts (a run's start and end, a dialog's
	// open and close, an interrupt included), so a terminal reading cannot know
	// more than it does. Pi's extension does; Claude's and Codex's hooks miss
	// interrupts, so a terminal reading outranks theirs.
	CompleteLifecycle bool
	// EventTimeOrder marks provider evidence that competes by exact event time
	// before kind (Codex): a newer hook beats an older snapshot of the same
	// conversation, and an older snapshot never repaints a newer hook.
	EventTimeOrder bool
	// AttentionWriter is the writer that raised Attention when it is not this
	// evidence's own: a Codex app-server sample composed with a request the
	// hooks hold open carries the hook's request, not one the app-server saw.
	// The zero value means the candidate's own kind and source. Only that
	// writer's own later event, or a provider snapshot, resolves the request.
	AttentionWriter Writer
	ObservedAt      time.Time
	// FreshUntil is the evidence's deadline. A terminal reading has none while
	// its pane is followed (zero: it holds until the next reading); every other
	// kind with a zero deadline is never fresh.
	FreshUntil time.Time
}

// Identity is what a candidate claims to be about.
type Identity struct {
	// Provider is the agent kind the evidence is for ("claude", "codex", "pi"),
	// or, for a terminal reading, the agent herdr detected in the pane ("" when
	// it detected none).
	Provider string
	// SessionID is the provider session (root) id. A terminal reading has none.
	SessionID string
	// StartedAt is the agent process lifetime the evidence was gathered for. A
	// terminal reading has none.
	StartedAt time.Time
	// PaneID is the terminal pane a terminal reading is of. It changes when
	// the pane moves to another workspace.
	PaneID string
	// TerminalID is herdr's id for the terminal a reading is of, stable across
	// pane moves; "" when herdr did not report one.
	TerminalID string
}

// Writer names the evidence that raised a request for the user.
type Writer struct {
	Kind   statusexplain.EvidenceKind
	Source agentgraph.SourceKind
}

// HookLatched marks c's attention as the provider hooks' own: a request a
// hook raised and holds open, carried by c because c was composed with it.
// A candidate with no attention is returned unchanged.
func HookLatched(c Candidate) Candidate {
	if c.Attention == agentgraph.AttentionNone || c.Attention == "" {
		return c
	}
	c.AttentionWriter = Writer{Kind: statusexplain.EvidenceHook, Source: agentgraph.SourceHook}
	return c
}

// writer is who raised c's attention: AttentionWriter, else c itself.
func (c Candidate) writer() Writer {
	if c.AttentionWriter.Kind != "" {
		return c.AttentionWriter
	}
	return Writer{Kind: c.Kind, Source: c.Source}
}

// Fresh reports whether c may decide at now: within its half-open freshness
// window, as agentgraph.Observation.Fresh defines it, except that a terminal
// reading with no deadline holds from its observation on.
func (c Candidate) Fresh(now time.Time) bool {
	if c.ObservedAt.IsZero() || now.Before(c.ObservedAt) {
		return false
	}
	if c.FreshUntil.IsZero() {
		return c.Kind == statusexplain.EvidenceTerminal
	}
	return now.Before(c.FreshUntil)
}

// The builders below turn one source's evidence into a candidate. Each is
// pure: a graph builder reduces its observation at the observation's own time,
// so its result does not depend on when it runs, and freshness is judged by
// Resolve against the caller's clock. startedAt is the agent process lifetime
// the evidence was gathered for. Each builder declares its candidate's source:
// the caller chose the builder for what the evidence is, so the graph's own
// source field (a Codex hook composed onto an app-server graph keeps the
// app-server's) is not consulted.

// ClaudeGraph is the Claude observer's transcript graph: a provider snapshot.
func ClaudeGraph(startedAt time.Time, o agentgraph.Observation) Candidate {
	return snapshot(agentgraph.SourceClaudeTranscript, startedAt, o, false)
}

// ClaudeHook is the graph a Claude hook landed: an exact lifecycle event. A
// Claude interrupt fires no hook, so its lifecycle is not complete.
func ClaudeHook(startedAt time.Time, o agentgraph.Observation) Candidate {
	return event(startedAt, o, false, false)
}

// CodexAppServer is a Codex app-server sample: a provider snapshot, ordered by
// event time against the conversation's other evidence.
func CodexAppServer(startedAt time.Time, o agentgraph.Observation) Candidate {
	return snapshot(agentgraph.SourceCodexAppServer, startedAt, o, true)
}

// CodexHook is a Codex root hook's observation: an exact lifecycle event,
// ordered by event time. Codex hooks miss interrupts, so it is not complete.
func CodexHook(startedAt time.Time, o agentgraph.Observation) Candidate {
	return event(startedAt, o, false, true)
}

// CodexChildHooks is the overlay of Codex child hooks that arrived before the
// app-server placed their threads: a partial hook edge. It reports the root's
// working descendants and nothing about the root itself.
func CodexChildHooks(startedAt time.Time, o agentgraph.Observation) Candidate {
	c := base(statusexplain.EvidenceHookEdge, startedAt, o)
	c.Source = agentgraph.SourceHook
	c.WorkingDescendants = workingDescendants(o)
	return c
}

// CodexRolloutTail is the root's state read from its rollout file, the one an
// accepted hook named: correlated transcript evidence, ordered by event time.
// The transcript-poll idle correction is this kind; it no longer borrows the
// graph's source.
func CodexRolloutTail(startedAt time.Time, o agentgraph.Observation) Candidate {
	c := transcript(startedAt, o)
	c.Source = agentgraph.SourceCodexRollout
	c.EventTimeOrder = true
	return c
}

// PiHook is the Pi extension's hook graph: an exact lifecycle event with
// complete coverage, red while any dialog is open.
func PiHook(startedAt time.Time, o agentgraph.Observation) Candidate {
	return event(startedAt, o, true, false)
}

// PiSessionTail is the tail of Pi's session file, read while no hook has
// reached the daemon: correlated transcript evidence.
func PiSessionTail(startedAt time.Time, o agentgraph.Observation) Candidate {
	c := transcript(startedAt, o)
	c.Source = agentgraph.SourcePiSessionFile
	return c
}

// RestoredLastKnown is a graph loaded from state.json across a daemon
// restart: presentation only, until the deadline it was persisted with. Being
// loaded renews nothing, and it holds no attention open.
func RestoredLastKnown(startedAt time.Time, o agentgraph.Observation) Candidate {
	c := base(statusexplain.EvidenceRestored, startedAt, o)
	c.Source = agentgraph.SourceRestoredLastKnown
	c.Status = reduce(o).LegacyStatus
	return c
}

// HerdrReading is one herdr reading of a pane, as the terminal backend reports
// it. Status is herdr's raw status: working, blocked, done, idle or unknown.
type HerdrReading struct {
	PaneID string
	// Agent is the agent herdr detected in the pane, "" when none.
	// TerminalID is herdr's id for the pane's terminal, stable across pane
	// moves; "" when herdr did not report one.
	TerminalID string
	Agent      string
	Status     string
	// Live is whether the pane's server is followed now; false withdraws the
	// reading.
	Live bool
	// Since is when herdr's status began.
	Since time.Time
}

// Herdr is a herdr reading: a coarse terminal reading of working, idle or
// blocked. While the pane is followed it has no deadline; a reading that is
// not followed is withdrawn, expiring at now. A start that is unset or after
// now is dated now.
func Herdr(r HerdrReading, now time.Time) Candidate {
	observedAt := r.Since
	if observedAt.IsZero() || observedAt.After(now) {
		observedAt = now
	}
	c := Candidate{
		Kind: statusexplain.EvidenceTerminal, Source: agentgraph.SourceHerdr,
		Identity:   Identity{Provider: r.Agent, PaneID: r.PaneID, TerminalID: r.TerminalID},
		Status:     herdrStatus(r.Status),
		Attention:  agentgraph.AttentionNone,
		ObservedAt: observedAt,
	}
	if !r.Live {
		c.FreshUntil = now
	}
	return c
}

// herdrStatus maps herdr's raw status onto the legacy one: blocked is
// permission, done is idle, unknown decides nothing.
func herdrStatus(raw string) string {
	switch raw {
	case "working":
		return agentgraph.LegacyWorking
	case "blocked":
		return agentgraph.LegacyPermission
	case "idle", "done":
		return agentgraph.LegacyIdle
	default:
		return ""
	}
}

func base(kind statusexplain.EvidenceKind, startedAt time.Time, o agentgraph.Observation) Candidate {
	return Candidate{
		Kind: kind, Source: o.Source,
		Identity:   Identity{Provider: string(o.Provider), SessionID: o.RootID, StartedAt: startedAt},
		Attention:  agentgraph.AttentionNone,
		ObservedAt: o.ObservedAt, FreshUntil: o.FreshUntil,
	}
}

func snapshot(source agentgraph.SourceKind, startedAt time.Time, o agentgraph.Observation, eventTime bool) Candidate {
	c := base(statusexplain.EvidenceProviderSnapshot, startedAt, o)
	c.Source = source
	summary := reduce(o)
	c.Status, c.Attention = summary.LegacyStatus, summary.Attention
	c.WorkingDescendants = workingDescendants(o)
	c.EventTimeOrder = eventTime
	return c
}

func event(startedAt time.Time, o agentgraph.Observation, complete, eventTime bool) Candidate {
	c := base(statusexplain.EvidenceHook, startedAt, o)
	// The writer is the hook, whatever graph it landed on: a Codex hook
	// composed onto an app-server graph keeps that graph's source.
	c.Source = agentgraph.SourceHook
	summary := reduce(o)
	c.Status, c.Attention = summary.LegacyStatus, summary.Attention
	// Claude's and Codex's hooks land on the graph their provider composed,
	// children included, so the event reports the descendants it carries.
	c.WorkingDescendants = workingDescendants(o)
	c.CompleteLifecycle = complete
	c.EventTimeOrder = eventTime
	return c
}

// transcript keeps only the root's runtime: a transcript tail establishes
// working or idle, never attention or delegation.
func transcript(startedAt time.Time, o agentgraph.Observation) Candidate {
	c := base(statusexplain.EvidenceTranscript, startedAt, o)
	switch reduce(o).Runtime {
	case agentgraph.RuntimeActive:
		c.Status = agentgraph.LegacyWorking
	case agentgraph.RuntimeIdle:
		c.Status = agentgraph.LegacyIdle
	}
	return c
}

// reduce is the neutral reducer's summary of o at its own observation time:
// what the evidence asserted, whether or not it is still fresh.
func reduce(o agentgraph.Observation) agentgraph.Summary {
	return agentgraph.Reduce(o, agentgraph.Summary{}, o.ObservedAt)
}

// workingDescendants counts o's positively live, working non-root nodes, the
// ones agentgraph.Reduce turns an idle root delegating for.
func workingDescendants(o agentgraph.Observation) int {
	normalized, err := agentgraph.Normalize(o)
	if err != nil {
		return 0
	}
	n := 0
	for _, node := range normalized.Nodes {
		if node.ID == normalized.RootID || !agentgraph.PositivelyLive(node) {
			continue
		}
		if node.Runtime == agentgraph.RuntimeActive || node.Lifecycle == agentgraph.LifecyclePending ||
			node.Lifecycle == agentgraph.LifecycleRunning {
			n++
		}
	}
	return n
}
