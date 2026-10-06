package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// Step 11 of docs/askuserquestion-model-plan.md §4: what a prompt is must survive
// a daemon restart, not just who owns it.
//
// Two defects, one schema change. The restore path stamped `approval` on every
// rebuilt prompt because state.PendingPrompt carried no attention at all, so every
// question-red came back an approval-red — the same colour today, a silent
// downgrade the moment anything distinguishes them, and this branch does. And the
// block carried ONE prompt per writer, so a writer that went down blocked on two
// parallel calls came back holding one: answering the survivor took the chip green
// with a real call still blocking. That is the missed RED the prompt container
// fixed, reappearing across a restart.
//
// These tests drive the real path end to end — hooks into a live coordinator, a
// real state.json on disk, Load and the stale sweep, then a restore into a fresh
// observer — because every one of the defects above lives in the seams between
// those layers rather than inside any one of them.

const (
	restartPID      = 7731
	restartSID      = "0b7d5b1e-6f2c-4c31-9a24-6d0a4d2f81ce"
	restartTeammate = "af5bd126402ac16c7"
)

// restartHarness is one Claude session's daemon-side state: a state.json on disk,
// a store over it, a coordinator and its observer. Rebooting builds a second one
// over the same file, which is exactly what a daemon restart is.
type restartHarness struct {
	path       string
	transcript string
	store      *state.Store
	sink       *history.Sink
	ref        provider.RootRef
	observer   *claudeprovider.Observer
}

func bootDaemon(t *testing.T, path, transcriptPath string) *restartHarness {
	t.Helper()
	store := state.New(path)
	h := &restartHarness{path: path, transcript: transcriptPath, store: store, sink: history.NewSink(history.Config{})}
	if transcriptPath == "" {
		h.ref = seedCoordinatorSession(store, restartPID, time.Now().Add(-time.Hour), state.AgentKindClaude, restartSID, "/project")
		h.transcript = seedCoordinatorTranscript(t, store, restartPID, restartSID)
		h.ref.Transcript = h.transcript
		return h
	}
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	dropStaleSessions(store, fakeProcSource{st: map[int]procState{restartPID: procAlive}}, h.sink, nil, transcript.DefaultTailBytes)
	sessions := store.Snapshot().Sessions
	if len(sessions) != 1 {
		t.Fatalf("reloaded %d sessions, want the one that was persisted", len(sessions))
	}
	ref, ok := providerRootRef(sessions[0])
	if !ok {
		t.Fatal("the reloaded session no longer resolves to a provider root")
	}
	h.ref = ref
	return h
}

// coordinator returns a coordinator over the harness's store, tracking its root.
func (h *restartHarness) coordinator(t *testing.T) *agentCoordinator {
	t.Helper()
	h.observer = claudeprovider.NewObserver(t.TempDir())
	c := newAgentCoordinator(h.store, h.sink, h.observer, nil)
	c.refreshTrackedRoots()
	t.Cleanup(c.Close)
	return c
}

// permissionRequest raises one prompt for writer (bare id; "" is the main thread).
func (h *restartHarness) permissionRequest(t *testing.T, c *agentCoordinator, writer, tool, hash string, at time.Time) {
	t.Helper()
	sess, ok := sessionForKey(h.store.Snapshot(), h.ref.Key())
	if !ok {
		t.Fatal("the Claude root was lost before the prompt")
	}
	c.HandleHook(rpc.Request{
		Agent: state.AgentKindClaude, Event: "PermissionRequest", SessionID: h.ref.ProviderSessionID,
		Transcript: h.ref.Transcript, AgentID: writer, ToolName: tool, ToolInputHash: hash, ObservedAt: at,
	}, sess)
}

// postToolUse answers one call.
func (h *restartHarness) postToolUse(t *testing.T, c *agentCoordinator, writer, tool, hash, callID string, at time.Time) {
	t.Helper()
	sess, ok := sessionForKey(h.store.Snapshot(), h.ref.Key())
	if !ok {
		t.Fatal("the Claude root was lost before the completion")
	}
	c.HandleHook(rpc.Request{
		Agent: state.AgentKindClaude, Event: "PostToolUse", SessionID: h.ref.ProviderSessionID,
		Transcript: h.ref.Transcript, AgentID: writer, ToolName: tool, ToolInputHash: hash,
		ToolUseID: callID, ObservedAt: at,
	}, sess)
}

func (h *restartHarness) claude(t *testing.T) *state.AgentInfo {
	t.Helper()
	sessions := h.store.Snapshot().Sessions
	if len(sessions) != 1 || sessions[0].Claude == nil {
		t.Fatalf("snapshot has no claude block: %+v", sessions)
	}
	return sessions[0].Claude
}

func (h *restartHarness) graph(t *testing.T) *state.AgentGraph {
	t.Helper()
	sessions := h.store.Snapshot().Sessions
	if len(sessions) != 1 || sessions[0].AgentGraph == nil {
		t.Fatalf("snapshot has no agent graph: %+v", sessions)
	}
	return sessions[0].AgentGraph
}

// recordsFor is the persisted set for one writer, in the order it was written.
// The writer is named in its WIRE spelling ("main" for the main thread) because
// every block a test reads comes off a snapshot, which is where that projection
// is stamped.
func recordsFor(info *state.AgentInfo, writer string) []state.PendingPromptRecord {
	var records []state.PendingPromptRecord
	for _, record := range info.PendingPrompts {
		if record.Writer == writer {
			records = append(records, record)
		}
	}
	return records
}

func TestPromptStatePersistsAcrossADaemonRestart(t *testing.T) {
	t.Run("should preserve user_input attention across restore", func(t *testing.T) {
		// The defect this closes: compatibilityFromState hardcoded AttentionApproval
		// because the block could not say otherwise, so a question came back as an
		// approval wait. Same chip colour, different reason — and the reason is what
		// the graph, the ring and every later rule read.
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		first := bootDaemon(t, path, "")
		// Dated in the past, as a real hook is: a persisted graph is only re-read
		// after it has expired, which is when a restore is the best evidence there is.
		now := time.Now().Add(-5 * time.Minute)
		first.permissionRequest(t, first.coordinator(t), "", "AskUserQuestion", "ask-1", now)

		if got := first.graph(t).Summary.Attention; got != agentgraph.AttentionUserInput {
			t.Fatalf("live attention = %q, want user_input before the restart even happens", got)
		}

		second := bootDaemon(t, path, first.transcript)
		second.coordinator(t).restoreClaude(second.ref, time.Now())

		if got := second.graph(t).Summary.Attention; got != agentgraph.AttentionUserInput {
			t.Errorf("restored attention = %q, want user_input — a question that comes back an approval is a silent downgrade", got)
		}
		if got := second.claude(t).Status; got != state.StatusPermission {
			t.Errorf("restored status = %q, want permission", got)
		}
	})

	t.Run("should preserve every one of a writer's parallel prompts across restore", func(t *testing.T) {
		// 7.6 % of tool-using turns dispatch two or more calls at once
		// (askuserquestion-model-plan.md §2.2). The legacy block held one prompt per
		// writer, so every restart collapsed such a set to its newest member.
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		first := bootDaemon(t, path, "")
		c := first.coordinator(t)
		now := time.Now().Add(-5 * time.Minute)
		first.permissionRequest(t, c, "", "Bash", "call-a", now)
		first.permissionRequest(t, c, "", "Bash", "call-b", now.Add(time.Second))
		first.permissionRequest(t, c, restartTeammate, "AskUserQuestion", "ask-1", now.Add(2*time.Second))

		if got := recordsFor(first.claude(t), state.PendingWriterMain); len(got) != 2 {
			t.Fatalf("persisted %d main-thread records, want both parallel prompts: %+v", len(got), got)
		}

		second := bootDaemon(t, path, first.transcript)
		second.coordinator(t).restoreClaude(second.ref, time.Now())

		restored := second.claude(t)
		if got := recordsFor(restored, state.PendingWriterMain); len(got) != 2 {
			t.Errorf("restored %d main-thread prompts, want 2 — the collapsed set is a green chip with a live call behind it", len(got))
		}
		if got := recordsFor(restored, restartTeammate); len(got) != 1 {
			t.Errorf("restored %d teammate prompts, want 1: %+v", len(got), got)
		}
		if got := len(restored.Pending); got != 2 {
			t.Errorf("restored %d blocked writers, want 2 (the main thread and the teammate)", got)
		}
		// The teammate's question and the main thread's approvals both survive, and
		// the summary folds them by the neutral reducer's own precedence.
		if got := second.graph(t).Summary.Attention; got != agentgraph.AttentionApproval {
			t.Errorf("restored attention = %q, want approval to win over the concurrent question", got)
		}
	})

	t.Run("should hold red after restore when only one of two restored prompts is answered", func(t *testing.T) {
		// The missed RED itself. Before the set was persisted the restart left one
		// record standing for two calls, so the first answer cleared the chip.
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		first := bootDaemon(t, path, "")
		c := first.coordinator(t)
		now := time.Now().Add(-5 * time.Minute)
		first.permissionRequest(t, c, "", "Bash", "call-a", now)
		first.permissionRequest(t, c, "", "Bash", "call-b", now.Add(time.Second))

		second := bootDaemon(t, path, first.transcript)
		restarted := second.coordinator(t)
		restarted.restoreClaude(second.ref, time.Now())

		second.postToolUse(t, restarted, "", "Bash", "call-b", "", now.Add(time.Minute))

		if got := second.claude(t).Status; got != state.StatusPermission {
			t.Errorf("status after answering one of two restored prompts = %q, want permission — the other call is still blocking", got)
		}
		if got := len(recordsFor(second.claude(t), state.PendingWriterMain)); got != 1 {
			t.Errorf("%d main-thread prompts remain, want exactly the unanswered one", got)
		}

		second.postToolUse(t, restarted, "", "Bash", "call-a", "", now.Add(2*time.Minute))
		if got := second.claude(t).Status; got == state.StatusPermission {
			t.Error("status stayed permission after both restored prompts were answered; a red must still be releasable")
		}
		if got := second.claude(t).PendingPrompts; len(got) != 0 {
			t.Errorf("%d records survived the last answer: %+v", len(got), got)
		}
	})

	t.Run("should subtract an exactly answered call from a mixed set during downtime", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		first := bootDaemon(t, path, "")
		live := first.coordinator(t)
		now := time.Now().Add(-5 * time.Minute)

		first.permissionRequest(t, live, "", "Bash", "hash-bash", now)
		appendTranscriptLine(t, first.transcript, toolUseLine(now.Add(-200*time.Millisecond), "Bash", "toolu_bash"))
		first.permissionRequest(t, live, "", "Edit", "hash-edit", now.Add(time.Second))
		appendTranscriptLine(t, first.transcript, toolUseLine(now.Add(800*time.Millisecond), "Edit", "toolu_edit"))

		ctx := context.Background()
		live.observeAt(ctx, first.ref, now.Add(4*time.Second))
		live.observeAt(ctx, first.ref, now.Add(7*time.Second))
		before := recordsFor(first.claude(t), state.PendingWriterMain)
		if len(before) != 2 || before[0].CallID == "" || before[1].CallID == "" {
			t.Fatalf("persisted records did not retain their confirmed call ids: %+v", before)
		}

		// No hook reaches the daemon for this result: it lands while switchboard
		// is down. The next boot must remove only toolu_bash and keep toolu_edit.
		appendTranscriptLine(t, first.transcript, toolResultLine(now.Add(10*time.Second), "toolu_bash"))
		second := bootDaemon(t, path, first.transcript)
		after := recordsFor(second.claude(t), state.PendingWriterMain)
		if len(after) != 1 || after[0].CallID != "toolu_edit" {
			t.Fatalf("records after downtime hydrate = %+v, want only toolu_edit", after)
		}
		if second.claude(t).Status != state.StatusPermission {
			t.Fatalf("status = %q, want permission while toolu_edit still blocks", second.claude(t).Status)
		}
	})

	t.Run("should load an old state.json without inventing or dropping a prompt", func(t *testing.T) {
		// The version boundary in both directions. A mirror written before
		// pending_prompts existed carries only the writer key set, and must restore
		// exactly the residual red it always did: one prompt for the owner, no
		// correlators, and nothing invented for a writer it never named.
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		transcriptPath := hydrateFixture(t, restartSID, map[string]string{restartTeammate: hydrateBlocked})
		legacy := `{"schema_version":` + strconv.Itoa(state.CurrentSchemaVersion) + `,"sessions":[{"pid":` + strconv.Itoa(restartPID) +
			`,"cwd":"/project","tty":"/dev/pts/3","started_at":"2026-08-30T09:00:00Z","agent":"claude","claude":` +
			`{"session_id":"` + restartSID + `","transcript":"` + transcriptPath + `","status":"permission",` +
			`"pending_writers":["` + restartTeammate + `"]}}],"updated_at":"2026-08-30T09:05:00Z"}`
		if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
			t.Fatal(err)
		}

		old := bootDaemon(t, path, transcriptPath)
		old.coordinator(t).restoreClaude(old.ref, time.Now())

		info := old.claude(t)
		if got := info.PendingWriterKeys(); len(got) != 1 || got[0] != restartTeammate {
			t.Fatalf("restored writers = %v, want just the persisted teammate", got)
		}
		if got := recordsFor(info, restartTeammate); len(got) != 1 {
			t.Errorf("old mirror restored %d records for its one writer, want exactly 1 — more is invented, fewer is a lost red", len(got))
		} else if got[0].Tool != "" || got[0].InputHash != "" {
			t.Errorf("old mirror restored correlators it never carried: %+v", got[0])
		}
		if got := info.Status; got != state.StatusPermission {
			t.Errorf("restored status = %q, want the persisted red held", got)
		}
		if got := old.graph(t).Summary.Attention; got != agentgraph.AttentionApproval {
			t.Errorf("restored attention = %q; a block with no attention degrades to approval, never to none", got)
		}
	})

	t.Run("should keep an older reader's view of the mirror intact", func(t *testing.T) {
		// Forward compatibility: pending_prompts is additive, so pending_writers must
		// still say exactly what it always said. An older daemon or bar reads only
		// that field, and it is the one that carries OWNERSHIP.
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		h := bootDaemon(t, path, "")
		c := h.coordinator(t)
		now := time.Now().Add(-5 * time.Minute)
		h.permissionRequest(t, c, "", "Bash", "call-a", now)
		h.permissionRequest(t, c, "", "Bash", "call-b", now.Add(time.Second))
		h.permissionRequest(t, c, restartTeammate, "AskUserQuestion", "ask-1", now.Add(2*time.Second))

		var mirror struct {
			Sessions []struct {
				Claude struct {
					PendingWriters []string                    `json:"pending_writers"`
					PendingPrompts []state.PendingPromptRecord `json:"pending_prompts"`
				} `json:"claude"`
			} `json:"sessions"`
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &mirror); err != nil {
			t.Fatalf("state.json is not decodable by a reader that knows only the old shape: %v", err)
		}
		writers := mirror.Sessions[0].Claude.PendingWriters
		if len(writers) != 2 || writers[0] != restartTeammate || writers[1] != state.PendingWriterMain {
			t.Errorf("pending_writers = %v, want the sorted key set unchanged by the new field", writers)
		}
		records := mirror.Sessions[0].Claude.PendingPrompts
		if len(records) != 3 {
			t.Fatalf("pending_prompts = %+v, want one record per open call", records)
		}
		// Grouped by writer in the same ascending order, oldest-first within a writer.
		if records[0].Writer != restartTeammate || records[1].Writer != state.PendingWriterMain {
			t.Errorf("pending_prompts writers = %q/%q/%q, want them grouped and sorted like pending_writers",
				records[0].Writer, records[1].Writer, records[2].Writer)
		}
		if records[0].Attention != string(agentgraph.AttentionUserInput) {
			t.Errorf("teammate record attention = %q, want the question's own kind", records[0].Attention)
		}
		if !records[1].Since.Before(records[2].Since) {
			t.Errorf("main-thread records are not oldest-first: %v then %v", records[1].Since, records[2].Since)
		}
	})

	t.Run("should clear a restored prompt on its own call's result rather than on the next assistant message", func(t *testing.T) {
		// The other half of persisting the set: a record names ONE call, so the
		// restored prompt may bind that call's id and be released by its own
		// tool_result. A prompt restored from the legacy scalar may not — it stands
		// for a writer's leftover red, not for one call — so before this change every
		// restored red had to wait for whole-file evidence.
		//
		// The result here is dated BEFORE the restore instant: the user answered while
		// the daemon was down, and the entry reached disk after it came back (Claude
		// Code dates an entry at generation time and flushes it seconds later). That
		// is what makes this decisive — the whole-file rules run from the restart
		// instant and can see nothing newer than it, so the only thing that can clear
		// this red is the id-matched result.
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		first := bootDaemon(t, path, "")
		now := time.Now().Add(-5 * time.Minute)
		first.permissionRequest(t, first.coordinator(t), "", "AskUserQuestion", "ask-1", now)
		appendTranscriptLine(t, first.transcript, toolUseLine(now.Add(-200*time.Millisecond), "AskUserQuestion", "toolu_restored"))

		second := bootDaemon(t, path, first.transcript)
		restarted := second.coordinator(t)
		ctx := context.Background()
		// Three driven ticks. The first performs the restore, which stamps its own
		// observation instant and supersedes the tick that triggered it. The other
		// two bind, and they must be a real interval apart: one read's uniqueness is
		// not uniqueness, so the earlier only PROPOSES the candidate and a
		// confirmation taken moments later is refused as the same view of the same
		// partial file. The clock is supplied rather than slept through.
		restoredAt := time.Now()
		restarted.observeAt(ctx, second.ref, restoredAt)
		restarted.observeAt(ctx, second.ref, restoredAt.Add(time.Second))
		restarted.observeAt(ctx, second.ref, restoredAt.Add(6*time.Second))
		if got := second.claude(t).Status; got != state.StatusPermission {
			t.Fatalf("status = %q while the call was still open, want permission held through the latch", got)
		}

		// It is the record's own onset, not the restart instant, that lets the latch
		// recognize a call dispatched before the daemon came back.
		bound := second.observer.Projection(second.ref.Key()).PendingSets[""]
		if len(bound) != 1 || bound[0].Latch != claudeprovider.CallLatchBound || bound[0].CallID != "toolu_restored" {
			t.Fatalf("restored prompt = %+v, want it bound to the call it gates", bound)
		}

		appendTranscriptLine(t, first.transcript, toolResultLine(now.Add(time.Second), "toolu_restored"))
		restarted.observeAt(ctx, second.ref, restoredAt.Add(11*time.Second))

		if got := second.claude(t).Status; got == state.StatusPermission {
			t.Error("the restored prompt outlived its own call's result; a restored record that names a call must be releasable by it")
		}
		if got := second.claude(t).PendingPrompts; len(got) != 0 {
			t.Errorf("%d records survived the answered call: %+v", len(got), got)
		}
	})
}

// toolUseLine is a writer's dispatch of one gated call — the entry the lazy latch
// reads to learn which call a prompt gates.
func toolUseLine(at time.Time, tool, callID string) string {
	return `{"type":"assistant","timestamp":"` + at.UTC().Format(time.RFC3339Nano) +
		`","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"` +
		callID + `","name":"` + tool + `"}]}}`
}

// toolResultLine is the user turn that answers exactly that call.
func toolResultLine(at time.Time, callID string) string {
	return `{"type":"user","timestamp":"` + at.UTC().Format(time.RFC3339Nano) +
		`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + callID + `"}]}}`
}
