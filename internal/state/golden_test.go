package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goldenPath is the frozen state.json schema fixture. It is the public
// contract documented in docs/state-schema.md; changing its shape is a
// breaking change to every bar/consumer that reads state.json.
var goldenPath = filepath.Join("testdata", "state.golden.json")

// canonicalSnapshot is the schema example: a fully-populated claude session
// (every optional block present, every AgentInfo field set), a codex session on
// the Observe tier (the additive "agent"/"codex" fields), and one minimal
// session (all optional blocks omitted, only the always-present fields). All
// timestamps are fixed and UTC so encode/decode is deterministic.
func canonicalSnapshot() Snapshot {
	return Snapshot{
		Sessions: []Session{
			{
				PID:       4821,
				CWD:       "/home/tjmisko/Projects/switchboard",
				TTY:       "/dev/pts/3",
				StartedAt: time.Date(2026, 5, 28, 9, 0, 0, 0, time.UTC),
				Focused:   true,
				Suspended: true,
				Agent:     AgentKindClaude,
				Wezterm: &WeztermInfo{
					MuxPID:      4790,
					MuxSocket:   "/run/user/1000/wezterm/gui-sock-4790",
					PaneID:      12,
					TabID:       7,
					WindowID:    3,
					WindowTitle: "claude — switchboard",
				},
				Hyprland: &HyprlandInfo{
					Address:     "0x5640f1a2b3c0",
					Workspace:   "4",
					WorkspaceID: 4,
					Monitor:     "DP-1",
				},
				Claude: &AgentInfo{
					SessionID:  "e0b4b21f-aaf6-4ab0-a8d6-2d595aba4065",
					Transcript: "/home/tjmisko/.claude/projects/switchboard/e0b4b21f.jsonl",
					Status:     "working",
				},
			},
			{
				PID:       4999,
				CWD:       "/home/tjmisko/Projects/api",
				TTY:       "/dev/pts/7",
				StartedAt: time.Date(2026, 5, 28, 9, 2, 0, 0, time.UTC),
				Focused:   false,
				Agent:     AgentKindCodex,
				Codex: &AgentInfo{
					SessionID:  "0199736b-b713-74e2-99a2-f015a1c42816",
					Transcript: "/home/tjmisko/.codex/sessions/2026/05/28/rollout-2026-05-28T09-02-00-0199736b-b713-74e2-99a2-f015a1c42816.jsonl",
					Status:     "idle",
				},
			},
			{
				PID:       5102,
				CWD:       "/home/tjmisko/Tools/other",
				TTY:       "/dev/pts/9",
				StartedAt: time.Date(2026, 5, 28, 9, 5, 0, 0, time.UTC),
				Focused:   false,
			},
		},
		UpdatedAt: time.Date(2026, 5, 28, 9, 5, 30, 0, time.UTC),
	}
}

// encodeSnapshot mirrors Store.persist exactly: two-space indent, trailing
// newline from Encode. The golden file must be byte-identical to this output.
func encodeSnapshot(snap Snapshot) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TestStateGoldenRoundTrips pins the public state.json wire format: the golden
// decodes into a Snapshot and re-encodes byte-for-byte. This catches any
// struct-tag, field-order, or omitempty change that would break consumers.
func TestStateGoldenRoundTrips(t *testing.T) {
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with UPDATE_GOLDEN=1 to create)", err)
	}

	var snap Snapshot
	if err := json.Unmarshal(want, &snap); err != nil {
		t.Fatalf("decode golden: %v", err)
	}

	got, err := encodeSnapshot(snap)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("round-trip mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestUpdateGolden regenerates the golden from canonicalSnapshot. It is a
// no-op unless UPDATE_GOLDEN is set, so it never fails CI; run
// `UPDATE_GOLDEN=1 go test ./internal/state` to refresh after a deliberate
// schema change.
func TestUpdateGolden(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") == "" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate testdata/state.golden.json")
	}
	b, err := encodeSnapshot(canonicalSnapshot())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(goldenPath, b, 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}
