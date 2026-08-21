package state_test

import (
	"path/filepath"
	"testing"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

func TestLoadHydratesPartialAgentGraphAxesAndOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	testsupport.WriteFile(t, path, `{
  "sessions": [{
    "pid": 7,
    "cwd": "/workspace",
    "tty": "/dev/pts/7",
    "started_at": "2026-08-21T15:00:00Z",
    "focused": false,
    "agent": "codex",
    "codex": {"session_id": "old", "status": "working"},
    "agent_graph": {
      "root_id": "root",
      "source": "restored_last_known",
      "observed_at": "2020-08-21T16:00:00Z",
      "fresh_until": "2099-08-21T16:01:00Z",
      "complete": false,
      "summary": {},
      "nodes": [
        {"id": "z", "parent_id": "root"},
        {"id": "root"},
        {"id": "a", "parent_id": "root"}
      ]
    }
  }],
  "updated_at": "2026-08-21T16:00:00Z"
}`)
	store := state.New(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess := store.Snapshot().Sessions[0]
	if sess.AgentGraph == nil {
		t.Fatal("partial but structurally valid graph was dropped")
	}
	wantIDs := []string{"root", "a", "z"}
	for i, want := range wantIDs {
		n := sess.AgentGraph.Nodes[i]
		if n.ID != want {
			t.Fatalf("node order[%d] = %q, want %q", i, n.ID, want)
		}
		if n.Runtime != agentgraph.RuntimeUnknown || n.Attention != agentgraph.AttentionNone || n.Lifecycle != agentgraph.LifecycleUnknown {
			t.Fatalf("node axes were not canonicalized: %+v", n)
		}
	}
	if sess.Codex.SessionID != "root" || sess.Codex.Status != "" {
		t.Fatalf("fresh hydrated compatibility block = %+v, want root id and unknown status", sess.Codex)
	}
	if sess.Codex.StatusSinceWire != nil {
		t.Fatalf("legacy status_since was recovered during hydration: %v", sess.Codex.StatusSinceWire)
	}
}

func TestLoadExpiresRestoredAgentGraphWithoutDroppingStructure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	testsupport.WriteFile(t, path, `{
  "sessions": [{
    "pid": 8,
    "cwd": "/workspace",
    "tty": "/dev/pts/8",
    "started_at": "2026-08-21T15:00:00Z",
    "focused": false,
    "agent": "codex",
    "codex": {"session_id": "root", "status": "permission", "status_since": "2000-01-01T00:00:00Z"},
    "agent_graph": {
      "root_id": "root",
      "source": "restored_last_known",
      "observed_at": "2000-01-01T00:00:00Z",
      "fresh_until": "2000-01-01T00:01:00Z",
      "complete": true,
      "summary": {"runtime":"idle", "attention":"approval", "status":"permission", "waiting_nodes":1, "approval_nodes":1, "since":"2000-01-01T00:00:00Z"},
      "nodes": [{"id":"root", "runtime":"idle", "attention":"approval", "lifecycle":"running"}]
    }
  }],
  "updated_at": "2000-01-01T00:00:00Z"
}`)
	store := state.New(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess := store.Snapshot().Sessions[0]
	if sess.AgentGraph == nil || len(sess.AgentGraph.Nodes) != 1 {
		t.Fatalf("expired graph structure was discarded: %+v", sess.AgentGraph)
	}
	if sess.AgentGraph.Summary.Status != "" || sess.AgentGraph.Summary.Runtime != agentgraph.RuntimeUnknown || sess.AgentGraph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("expired graph remained authoritative: %+v", sess.AgentGraph.Summary)
	}
	if sess.Codex.Status != "" || sess.Codex.StatusSinceWire != nil {
		t.Fatalf("expired graph kept legacy status authoritative: %+v", sess.Codex)
	}
}

func TestLoadDropsStructurallyInvalidAgentGraphSafely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	testsupport.WriteFile(t, path, `{
  "sessions": [{
    "pid": 9,
    "cwd": "/workspace",
    "tty": "/dev/pts/9",
    "started_at": "2026-08-21T15:00:00Z",
    "focused": false,
    "agent": "codex",
    "codex": {"status": "idle"},
    "agent_graph": {"root_id":"missing", "nodes":[{"id":"other"}]}
  }],
  "updated_at": "2026-08-21T16:00:00Z"
}`)
	store := state.New(path)
	if err := store.Load(); err != nil {
		t.Fatalf("invalid optional graph should not poison the whole legacy snapshot: %v", err)
	}
	if graph := store.Snapshot().Sessions[0].AgentGraph; graph != nil {
		t.Fatalf("invalid graph survived hydration: %+v", graph)
	}
}
