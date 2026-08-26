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
// (every optional block present, every optional scalar and AgentInfo field set
// — deliberately maximal for coverage, not a realistic combination), a codex
// session on the Observe tier (the additive "agent"/"codex" fields), and one minimal session
// (all optional blocks omitted, only the always-present fields — it is what
// pins the omitempty ABSENCE of the optional fields). All timestamps are fixed
// and UTC so encode/decode is deterministic.
//
// Every optional field MUST be set on at least one session here. An omitempty
// field that is never set is invisible to the golden, and
// TestStateGoldenRoundTrips cannot see the gap (it round-trips the fixture
// against itself); TestStateGoldenPinsCanonicalSnapshot is what forces this
// function and the fixture to agree.
func canonicalSnapshot() Snapshot {
	return Snapshot{
		SchemaVersion: CurrentSchemaVersion,
		Sessions: []Session{
			{
				PID:       4821,
				CWD:       "/home/tjmisko/Projects/switchboard",
				TTY:       "/dev/pts/3",
				StartedAt: time.Date(2026, 5, 28, 9, 0, 0, 0, time.UTC),
				Focused:   true,
				Suspended: true,
				Headless:  true,
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
					// status_since: the wire projection of StatusSince, present once a
					// status edge has stamped it (omitted before then — see the codex/
					// minimal sessions). Renderers compute "working 3m" from it.
					StatusSinceWire: timePtr(time.Date(2026, 5, 28, 9, 1, 0, 0, time.UTC)),
					// in_flight_subagents: the S dimension. Set here so the fixture
					// pins the field at all; >0 with an *idle* main thread is what
					// promotes a session to "delegating".
					InFlightSubagents: 2,
					// pending_writers: the wire projection of the Pending map's key
					// set — which WRITERS are blocked on a permission prompt, with
					// "main" standing in for the main thread. Sorted ascending, and
					// deliberately set here in its already-projected form: this
					// function feeds the encoder directly rather than going through
					// enrichForWire, exactly as StatusSinceWire above does.
					PendingWriters: []string{"af5bd126402ac16c7", "main"},
					// workflows: the active ultracode Workflow runs behind the
					// in_flight_subagents count — set here to pin the field and its
					// nested shape on the wire. Sorted by run_id.
					Workflows: []WorkflowStatus{{
						RunID:         "wf_5e3cb808-2ac",
						Name:          "simplification-audit",
						AgentsStarted: 17,
						AgentsDone:    7,
						InFlight:      2,
					}},
				},
			},
			{
				PID:       4999,
				CWD:       "/home/tjmisko/Projects/api",
				TTY:       "/dev/pts/7",
				StartedAt: time.Date(2026, 5, 28, 9, 2, 0, 0, time.UTC),
				Focused:   false,
				Agent:     AgentKindCodex,
				DisplayName: &DisplayName{
					Value: "api-cleanup", Origin: DisplayNameGenerated,
					ConversationID: "0199736b-b713-74e2-99a2-f015a1c42816",
					NativeBaseline: stringPtr("API maintenance"),
				},
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

// timePtr returns the address of t, for the optional *time.Time wire fields.
func timePtr(t time.Time) *time.Time { return &t }
func stringPtr(value string) *string { return &value }

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

// TestStateGoldenPinsCanonicalSnapshot closes the hole in the round-trip test
// above: that one decodes the fixture and re-encodes it, so an omitempty field
// missing from the fixture decodes to zero and re-encodes to absent — green
// whether or not the field exists on the struct at all. A newly added optional
// field is therefore unpinned until it reaches the fixture, which is how
// in_flight_subagents and headless came to sit on the wire while the golden
// said nothing about them. Encoding canonicalSnapshot and demanding the fixture
// match makes the golden a real tripwire: add a field, set it in
// canonicalSnapshot, and this fails until you run
// `UPDATE_GOLDEN=1 go test ./internal/state` and commit the result.
func TestStateGoldenPinsCanonicalSnapshot(t *testing.T) {
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with UPDATE_GOLDEN=1 to create)", err)
	}

	got, err := encodeSnapshot(canonicalSnapshot())
	if err != nil {
		t.Fatalf("encode canonical snapshot: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("golden is stale — regenerate with UPDATE_GOLDEN=1 go test ./internal/state.\n--- canonicalSnapshot ---\n%s\n--- golden ---\n%s", got, want)
	}
}

// TestBroadcastEncodingIsTheGoldenDocument extends the golden discipline to the
// bytes a subscriber actually receives. Store.broadcast now encodes each snapshot
// ONCE (marshalSnapshot) and every subscriber sends those same bytes, spliced
// into the response envelope verbatim by rpc — so that buffer IS the public
// document in docs/state-schema.md, merely without state.json's indentation.
// Compacting the golden and demanding the broadcast encoding match is what stops
// the shared-buffer path from quietly diverging from the file the schema
// describes: a change to escaping, field order, or omitempty fails here as loudly
// as it does for the on-disk mirror.
func TestBroadcastEncodingIsTheGoldenDocument(t *testing.T) {
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with UPDATE_GOLDEN=1 to create)", err)
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, want); err != nil {
		t.Fatalf("compact golden: %v", err)
	}

	got, err := marshalSnapshot(canonicalSnapshot())
	if err != nil {
		t.Fatalf("marshal canonical snapshot: %v", err)
	}

	if !bytes.Equal(got, compacted.Bytes()) {
		t.Errorf("broadcast encoding differs from the golden document.\n--- broadcast ---\n%s\n--- golden (compacted) ---\n%s", got, compacted.Bytes())
	}
}

// TestChangeKeyIgnoresUpdatedAtOnly pins the exact boundary of the publish gate:
// the change key must drop updated_at (snapshotLocked re-stamps it time.Now() on
// every snapshot, so comparing it would make the gate a no-op) and must keep
// everything else the golden document carries. Building it from the golden's own
// bytes means a newly added wire field is covered the moment the fixture is
// regenerated — the same tripwire that guards the file itself.
func TestChangeKeyIgnoresUpdatedAtOnly(t *testing.T) {
	base := canonicalSnapshot()

	restamped := base
	restamped.UpdatedAt = base.UpdatedAt.Add(5 * time.Second)
	if !bytes.Equal(snapshotChangeKey(base), snapshotChangeKey(restamped)) {
		t.Error("a fresh updated_at changed the key; the publish gate would never suppress anything")
	}

	// Every other kind of edit must move the key. status_since is the one to watch:
	// it is stamped from a wall clock, but only on a status edge, so it is a real
	// change and must NOT be excluded (see snapshotChangeKey).
	for name, mutate := range map[string]func(*Snapshot){
		"a session appearing": func(s *Snapshot) { s.Sessions = append(s.Sessions, Session{PID: 9001}) },
		"a focus change":      func(s *Snapshot) { s.Sessions[0].Focused = !s.Sessions[0].Focused },
		"a status edge":       func(s *Snapshot) { s.Sessions[0].Claude.Status = StatusIdle },
		"status_since moving": func(s *Snapshot) { s.Sessions[0].Claude.StatusSinceWire = timePtr(base.UpdatedAt) },
		"a display name":      func(s *Snapshot) { s.Sessions[1].DisplayName.Value = "new-display-name" },
		"a subagent landing":  func(s *Snapshot) { s.Sessions[0].Claude.InFlightSubagents++ },
		"a window moving":     func(s *Snapshot) { s.Sessions[0].Hyprland.WorkspaceID = 9 },
		"capabilities flipping": func(s *Snapshot) {
			s.Capabilities = &Capabilities{Observe: true, Navigate: true, WM: "sway", Terminal: "tmux"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := canonicalSnapshot()
			mutate(&changed)
			if bytes.Equal(snapshotChangeKey(base), snapshotChangeKey(changed)) {
				t.Errorf("%s did not move the change key; the bar would never see it", name)
			}
		})
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
