package fanout

import (
	"errors"
	"os"
	"sort"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// Lifecycle is the provider-local child lifecycle reported by Snapshot. The
// Claude adapter translates these values to the provider-neutral agent graph;
// keeping the scan result local lets fanout retain paths and suspect reasons
// that must never enter the public graph contract.
type Lifecycle string

const (
	LifecyclePending     Lifecycle = "pending"
	LifecycleRunning     Lifecycle = "running"
	LifecycleCompleted   Lifecycle = "completed"
	LifecycleInterrupted Lifecycle = "interrupted"
)

// SuspectReason explains a conservative force-close. It is deliberately a
// finite, content-free value suitable for diagnostics and compatibility
// history; it never contains transcript text or tool input.
type SuspectReason string

const (
	SuspectNone           SuspectReason = ""
	SuspectStaleQuiescent SuspectReason = "stale_quiescent"
)

// Root is the minimal legacy identity needed to scan one Claude session and to
// construct the existing subagent/workflow history events.
type Root struct {
	SessionID  string
	Transcript string
	PID        int
	Agent      string
	CWD        string
}

// Child is one stable Claude fanout record. TranscriptPath is adapter-internal
// evidence and must not be copied to the neutral graph or public state wire.
type Child struct {
	ID             string
	ParentID       string
	AgentType      string
	Nickname       string
	Description    string
	ToolUseID      string
	SpawnDepth     int
	TaskKind       string
	Background     bool
	SpawnedAt      time.Time
	StartedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    time.Time
	Lifecycle      Lifecycle
	SuspectReason  SuspectReason
	WorkflowRunID  string
	WorkflowName   string
	TranscriptPath string
}

// Workflow is the structured twin of the existing state.WorkflowStatus
// compatibility projection.
type Workflow struct {
	RunID         string
	Name          string
	AgentsStarted int
	AgentsDone    int
	InFlight      int
}

// Snapshot is a detached, deterministic view of one session's append-only
// fanout artifacts. Complete means both the flat subagent directory and all
// workflow journals were readable at ObservedAt.
type Snapshot struct {
	SessionID  string
	ObservedAt time.Time
	Complete   bool
	InFlight   int
	Children   []Child
	Workflows  []Workflow
}

// Clone returns a fully detached structured snapshot.
func (s Snapshot) Clone() Snapshot {
	clone := s
	clone.Children = append([]Child(nil), s.Children...)
	clone.Workflows = append([]Workflow(nil), s.Workflows...)
	return clone
}

// Result couples a structured snapshot with the exact-once legacy history
// events produced by the same reconcile trigger.
type Result struct {
	Snapshot Snapshot
	Events   []history.Event
}

// Observe returns a structured fanout snapshot while preserving Reconcile as
// the legacy state/history projection. Calls are restart-idempotent because the
// same per-session cursor and history-seeded seen sets back both APIs.
func (o *Observer) Observe(root Root, now time.Time) (Result, error) {
	if root.SessionID == "" || root.Transcript == "" {
		return Result{}, errors.New("fanout: session id and transcript are required")
	}
	sess := &state.Session{PID: root.PID, Agent: root.Agent, CWD: root.CWD}
	info := &state.AgentInfo{SessionID: root.SessionID, Transcript: root.Transcript}
	events := o.Reconcile(sess, info, now)

	o.mu.Lock()
	defer o.mu.Unlock()
	ss := o.sessions[root.SessionID]
	if ss == nil {
		return Result{}, errors.New("fanout: session was not initialized")
	}
	snapshot, err := o.snapshotLocked(root, ss, now)
	if err != nil {
		return Result{Snapshot: snapshot, Events: append([]history.Event(nil), events...)}, err
	}
	return Result{Snapshot: snapshot.Clone(), Events: append([]history.Event(nil), events...)}, nil
}

func (o *Observer) snapshotLocked(root Root, ss *sessionState, now time.Time) (Snapshot, error) {
	snapshot := Snapshot{SessionID: root.SessionID, ObservedAt: now, Complete: true}
	subs, err := transcript.SubagentsForTranscript(root.Transcript)
	if err != nil {
		snapshot.Complete = false
		return snapshot, err
	}
	for _, sub := range subs {
		if sub.AgentID == "" {
			continue
		}
		child := Child{
			ID: sub.AgentID, AgentType: sub.AgentType, Nickname: sub.Name,
			Description: sub.Description, ToolUseID: sub.ToolUseID,
			SpawnDepth: sub.SpawnDepth, TaskKind: sub.TaskKind,
			UpdatedAt:      sub.ModTime,
			TranscriptPath: transcript.SubagentPath(root.Transcript, sub.AgentID),
		}
		child.SpawnedAt = ss.spawnedAt[sub.AgentID]
		if child.SpawnedAt.IsZero() {
			child.SpawnedAt = subagentSpawnTime(root.Transcript, sub)
		}
		child.StartedAt = child.SpawnedAt
		child.Background = sub.ToolUseID != "" && ss.background[sub.ToolUseID]

		done := sub.Done
		if !done && sub.ToolUseID != "" && !child.Background && ss.resultDone[sub.ToolUseID] {
			done = true
		}
		stale := !done && !sub.ModTime.IsZero() && now.Sub(sub.ModTime) > o.staleCap
		switch {
		case stale:
			child.Lifecycle = LifecycleInterrupted
			child.SuspectReason = ss.suspect[sub.AgentID]
			child.CompletedAt = ss.completedAt[sub.AgentID]
		case done:
			child.Lifecycle = LifecycleCompleted
			child.CompletedAt = ss.completedAt[sub.AgentID]
			if child.CompletedAt.IsZero() {
				child.CompletedAt = now
			}
		case child.TranscriptPath == "" || !pathExists(child.TranscriptPath):
			child.Lifecycle = LifecyclePending
		default:
			child.Lifecycle = LifecycleRunning
		}
		if !child.LifecycleTerminal() && sub.SpawnDepth < 2 {
			snapshot.InFlight++
		}
		snapshot.Children = append(snapshot.Children, child)
	}

	runs, err := transcript.WorkflowRunsForTranscript(root.Transcript)
	if err != nil {
		snapshot.Complete = false
		return snapshot, err
	}
	for _, run := range runs {
		wc := ss.workflows[run.RunID]
		if wc == nil {
			continue
		}
		name := wc.name
		if name == "" {
			name = run.Name
		}
		running := 0
		for id := range wc.started {
			child := Child{
				ID: id, AgentType: transcript.WorkflowAgentType,
				WorkflowRunID: run.RunID, WorkflowName: name,
				TranscriptPath: run.AgentTranscript(id), SpawnDepth: 1,
			}
			child.SpawnedAt = ss.spawnedAt[id]
			child.StartedAt = child.SpawnedAt
			if mt, ok := workflowAgentMtime(run, id); ok {
				child.UpdatedAt = mt
			}
			switch {
			case wc.resulted[id]:
				child.Lifecycle = LifecycleCompleted
				child.CompletedAt = ss.completedAt[id]
				if child.CompletedAt.IsZero() {
					child.CompletedAt = now
				}
			case wc.closed[id]:
				child.Lifecycle = LifecycleInterrupted
				child.SuspectReason = ss.suspect[id]
				child.CompletedAt = ss.completedAt[id]
			default:
				child.Lifecycle = LifecycleRunning
				running++
			}
			snapshot.Children = append(snapshot.Children, child)
		}
		active := running > 0 || workflowFresh(run, now, o.quietGrace)
		if active {
			snapshot.Workflows = append(snapshot.Workflows, Workflow{
				RunID: run.RunID, Name: name, AgentsStarted: len(wc.started),
				AgentsDone: len(wc.resulted), InFlight: running,
			})
			snapshot.InFlight += running
		}
	}

	// Flat and workflow directories are each name-sorted, but their union needs
	// one final stable ordering. Workflow identity breaks the unlikely duplicate
	// agent-id tie without depending on filesystem traversal order.
	sort.Slice(snapshot.Children, func(i, j int) bool {
		left, right := snapshot.Children[i], snapshot.Children[j]
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		return left.WorkflowRunID < right.WorkflowRunID
	})
	sort.Slice(snapshot.Workflows, func(i, j int) bool {
		return snapshot.Workflows[i].RunID < snapshot.Workflows[j].RunID
	})
	return snapshot, nil
}

func (c Child) LifecycleTerminal() bool {
	return c.Lifecycle == LifecycleCompleted || c.Lifecycle == LifecycleInterrupted
}

func subagentSpawnTime(mainTranscript string, sub transcript.Subagent) time.Time {
	if path := transcript.SubagentMetaPath(mainTranscript, sub.AgentID); path != "" {
		if fi, err := os.Stat(path); err == nil {
			return fi.ModTime()
		}
	}
	return sub.ModTime
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func workflowFresh(run transcript.WorkflowRun, now time.Time, quietGrace time.Duration) bool {
	if mt, ok := statMtime(run.Journal); ok {
		return now.Sub(mt) <= quietGrace
	}
	if mt, ok := statMtime(run.Dir); ok {
		return now.Sub(mt) <= quietGrace
	}
	return false
}

// Forget drops one session's cursors and structured lifecycle cache. A resumed
// session re-seeds from history on its next observation.
func (o *Observer) Forget(sessionID string) {
	if sessionID == "" {
		return
	}
	o.mu.Lock()
	delete(o.sessions, sessionID)
	o.mu.Unlock()
}
