package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// testTune is the default tuning the reconciler tests run against (30s TTL,
// delegating on, resume→working, interrupt→idle).
var testTune = statustune.Default()

// TestPermissionExit pins the §5 case table for how a red chip exits: resolution
// KIND (not just "resolved?") plus the subagent count select the exit color.
func TestPermissionExit(t *testing.T) {
	ttl := testTune.PermissionDecayTTL
	tests := []struct {
		name       string
		kind       transcript.ResolutionKind
		unreadable bool
		age        time.Duration
		subagents  int
		wantExit   string
		wantOK     bool
		wantRule   string
	}{
		{
			name: "resumed → working, green, directly (case 9, no orange bounce)",
			kind: transcript.ResolutionResumed, age: 0, wantExit: "working", wantOK: true, wantRule: "case9-approve-resume",
		},
		{
			name: "interrupted with no teammates → idle, orange (case 10)",
			kind: transcript.ResolutionInterrupted, age: 0, wantExit: "idle", wantOK: true, wantRule: "case10-decline-idle",
		},
		{
			name: "interrupted with teammates in flight → delegating, green (case 11/Q3)",
			kind: transcript.ResolutionInterrupted, subagents: 2, wantExit: "delegating", wantOK: true, wantRule: "case11-decline-delegating",
		},
		{
			name: "none (readable, nothing resolved) → keep red, regardless of age",
			kind: transcript.ResolutionNone, age: time.Hour, wantOK: false,
		},
		{
			name:       "unreadable within ttl → keep red",
			unreadable: true, age: ttl - time.Second, wantOK: false,
		},
		{
			name:       "unreadable past ttl → interrupt color backstop (case 15)",
			unreadable: true, age: ttl + time.Second, wantExit: "idle", wantOK: true, wantRule: "case15-ttl-backstop",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exit, rule, _, ok := permissionExit(tt.kind, tt.unreadable, tt.age, tt.subagents, testTune)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if exit != tt.wantExit {
				t.Errorf("exit = %q, want %q", exit, tt.wantExit)
			}
			if rule != tt.wantRule {
				t.Errorf("rule = %q, want %q", rule, tt.wantRule)
			}
		})
	}
}

// The self-heal demotion is the one status edge with no hook behind it, so it
// must leave its own log trail naming the deciding reason. A preserved (still
// pending) chip stays silent.
func TestSelfHealStaleAttentionLogsDecay(t *testing.T) {
	since := mustParse(t, "2026-06-01T21:39:00Z")
	now := since.Add(time.Minute)

	var buf bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&buf)

	m := permMap(writeTranscript(t, tInterrupt("2026-06-01T21:39:30Z")), since)
	m[100].Claude.SessionID = "ce13c0f2-aaaa"
	selfHealStaleAttention(m, now, testTune, nil)
	if !strings.Contains(buf.String(), "status: pid=100 session=ce13c0f2 permission->idle rule=case10-decline-idle") {
		t.Errorf("missing decay log line in:\n%s", buf.String())
	}

	// Concurrent subagent/parallel tool_results land after the prompt but do not
	// resolve it, so the chip stays red and nothing is logged.
	buf.Reset()
	m = permMap(writeTranscript(t, tResult("2026-06-01T21:39:30Z")), since)
	selfHealStaleAttention(m, now, testTune, nil)
	if buf.Len() != 0 {
		t.Errorf("preserved chip logged %q, want silence", buf.String())
	}
}

func TestSelfHealStuckStatus(t *testing.T) {
	since := mustParse(t, "2026-06-01T21:39:00Z")
	now := since.Add(time.Minute)

	t.Run("should flip idle to working when conversational activity is newer than the chip", func(t *testing.T) {
		var buf bytes.Buffer
		defer log.SetOutput(log.Writer())
		log.SetOutput(&buf)

		m := stuckMap("idle", writeTranscript(t, tResult("2026-06-01T21:39:30Z")), since)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "working" {
			t.Fatalf("status = %q, want working", got)
		}
		if !m[100].Claude.StatusSince.Equal(now) {
			t.Errorf("StatusSince = %v, want now %v", m[100].Claude.StatusSince, now)
		}
		if !strings.Contains(buf.String(), "status: pid=100 session=ce13c0f2 idle->working rule=resume-activity") {
			t.Errorf("missing idle->working log in:\n%s", buf.String())
		}
	})

	t.Run("should keep idle when the newest activity predates the chip", func(t *testing.T) {
		m := stuckMap("idle", writeTranscript(t, tAssistant("2026-06-01T21:38:00Z")), since)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (no fresh activity)", got)
		}
	})

	t.Run("should keep idle when a bash command runs after the chip went idle", func(t *testing.T) {
		// The reported bug: a Stop fired (chip→idle), then the user ran `!git status`
		// in the session. Claude Code flushed a <bash-stdout> user entry dated after
		// the chip went idle, which read as activity and promoted the correctly-orange
		// chip back to green — where it latched, since a `!` command fires no Stop hook
		// to bring it back down. A local command must not count as a resume signal.
		m := stuckMap("idle", writeTranscript(t, tAssistant("2026-06-01T21:38:00Z"), tBash("2026-06-01T21:39:40Z")), since)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (a bash command is not agent activity)", got)
		}
	})

	t.Run("should keep idle when only timestamp-less metadata was written", func(t *testing.T) {
		// The metadata entry bumps the file mtime (so the read happens) but carries
		// no timestamp; the newest conversational entry predates the chip, so it
		// must not be mistaken for fresh activity.
		m := stuckMap("idle", writeTranscript(t, tAssistant("2026-06-01T21:38:00Z"), tMeta), since)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (metadata is not activity)", got)
		}
	})

	t.Run("should flip working to idle when the turn was interrupted", func(t *testing.T) {
		var buf bytes.Buffer
		defer log.SetOutput(log.Writer())
		log.SetOutput(&buf)

		m := stuckMap("working", writeTranscript(t, tInterrupt("2026-06-01T21:39:30Z")), since)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Fatalf("status = %q, want idle", got)
		}
		if !strings.Contains(buf.String(), "status: pid=100 session=ce13c0f2 working->idle rule=case6-interrupt") {
			t.Errorf("missing working->idle log in:\n%s", buf.String())
		}
	})

	t.Run("should keep working during ongoing activity with no interrupt", func(t *testing.T) {
		// A long tool run / active turn writes activity, never the interrupt marker,
		// so a busy session is never falsely decayed.
		m := stuckMap("working", writeTranscript(t, tAssistant("2026-06-01T21:39:30Z"), tResult("2026-06-01T21:39:40Z")), since)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "working" {
			t.Errorf("status = %q, want working (no interrupt → no decay)", got)
		}
	})

	t.Run("should skip the read when nothing was written since the chip transitioned", func(t *testing.T) {
		// Pre-gate: the transcript holds fresh activity, but its mtime is older than
		// StatusSince, so the read is skipped and the chip is left as-is.
		path := writeTranscript(t, tResult("2026-06-01T21:39:30Z"))
		old := since.Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		m := stuckMap("idle", path, since)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (mtime pre-gate skips the read)", got)
		}
	})

	t.Run("should leave a permission session untouched", func(t *testing.T) {
		m := stuckMap("permission", writeTranscript(t, tResult("2026-06-01T21:39:30Z")), since)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "permission" {
			t.Errorf("status = %q, want permission (owned by selfHealStaleAttention)", got)
		}
	})

	t.Run("should not re-flip a session decayed permission->idle in the same tick", func(t *testing.T) {
		// Mirrors reconcileOnce ordering: a declined permission decays to idle with
		// StatusSince=now, and the resolving interrupt notice (older than now) must
		// not then be read as fresh activity by selfHealStuckStatus.
		m := permMap(writeTranscript(t, tInterrupt("2026-06-01T21:39:30Z")), since)
		selfHealStaleAttention(m, now, testTune, nil)
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (no immediate re-flip to working)", got)
		}
	})

	// Delegating (cases 5/14, complaint #2): an idle main thread with teammates in
	// flight is green, and the promotion is decided from the S dimension WITHOUT a
	// fresh transcript write — the very case the mtime pre-gate would otherwise skip.
	t.Run("should promote idle to delegating when subagents are in flight", func(t *testing.T) {
		var buf bytes.Buffer
		defer log.SetOutput(log.Writer())
		log.SetOutput(&buf)

		// Stale mtime so the activity pre-gate would skip — proves delegating is
		// decided from the S dimension, not a transcript read.
		path := writeTranscript(t, tAssistant("2026-06-01T21:30:00Z"))
		old := since.Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		m := stuckMap("idle", path, since)
		m[100].Claude.InFlightSubagents = 2
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "delegating" {
			t.Fatalf("status = %q, want delegating", got)
		}
		if !strings.Contains(buf.String(), "status: pid=100 session=ce13c0f2 idle->delegating rule=case5-delegating") {
			t.Errorf("missing idle->delegating log in:\n%s", buf.String())
		}
	})

	t.Run("should revert delegating to idle when the last teammate drains", func(t *testing.T) {
		m := stuckMap("delegating", writeTranscript(t, tAssistant("2026-06-01T21:30:00Z")), since)
		m[100].Claude.InFlightSubagents = 0
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (subagents drained)", got)
		}
	})

	t.Run("should keep delegating while subagents remain in flight", func(t *testing.T) {
		m := stuckMap("delegating", writeTranscript(t, tAssistant("2026-06-01T21:30:00Z")), since)
		m[100].Claude.InFlightSubagents = 1
		selfHealStuckStatus(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "delegating" {
			t.Errorf("status = %q, want delegating (still in flight)", got)
		}
	})

	t.Run("should not promote idle to delegating when delegating is tuned off", func(t *testing.T) {
		tun := testTune
		tun.DelegatingEnabled = false
		m := stuckMap("idle", writeTranscript(t, tAssistant("2026-06-01T21:30:00Z")), since)
		m[100].Claude.InFlightSubagents = 2
		selfHealStuckStatus(m, now, tun, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (delegating disabled)", got)
		}
	})
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

// transcript fixtures (same wire shape as a live Claude Code .jsonl). A
// resolution is a timestamped user entry carrying a tool_result.
func tResult(ts string) string {
	return `{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X","is_error":true}]}}`
}

func tAssistant(ts string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`
}

func tInterrupt(ts string) string {
	return `{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
}

// tBash is the synthetic user entry Claude Code writes for a `!` bash command's
// output. It runs no agent turn, so it must not flip an idle chip to working.
func tBash(ts string) string {
	return `{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":"<bash-stdout>On branch main</bash-stdout>"}}`
}

// tMeta is a timestamp-less metadata entry — the kind that bumps the transcript
// mtime without representing work.
const tMeta = `{"type":"mode","mode":"acceptEdits"}`

// stuckMap builds a single-session map in the given status, as the reconcile
// Apply would pass to selfHealStuckStatus.
func stuckMap(status, transcriptPath string, since time.Time) map[int]*state.Session {
	return map[int]*state.Session{
		100: {PID: 100, Claude: &state.ClaudeInfo{
			Status:      status,
			Transcript:  transcriptPath,
			StatusSince: since,
			SessionID:   "ce13c0f2-aaaa",
		}},
	}
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

// permMap builds a session map holding a single "permission" session, as the
// reconcile Apply would pass to selfHealStaleAttention.
func permMap(transcriptPath string, since time.Time) map[int]*state.Session {
	return map[int]*state.Session{
		100: {
			PID: 100,
			Claude: &state.ClaudeInfo{
				Status:      "permission",
				Transcript:  transcriptPath,
				StatusSince: since,
			},
		},
	}
}

func TestSelfHealStaleAttention(t *testing.T) {
	since := mustParse(t, "2026-06-01T21:39:00Z")
	now := since.Add(time.Minute) // "now" is shortly after the prompt appeared

	t.Run("should demote permission to idle when an interrupt notice lands after the prompt (declined)", func(t *testing.T) {
		m := permMap(writeTranscript(t, tInterrupt("2026-06-01T21:39:30Z")), since)
		selfHealStaleAttention(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle", got)
		}
	})

	t.Run("should exit permission to WORKING (green) when an assistant message advances past the prompt (approved → turn resumed)", func(t *testing.T) {
		// A1/P3: an approved prompt's turn resuming goes straight to green, not
		// through orange — work is happening again, no action needed.
		m := permMap(writeTranscript(t, tAssistant("2026-06-01T21:39:30Z")), since)
		selfHealStaleAttention(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "working" {
			t.Errorf("status = %q, want working (approved → resumed → green)", got)
		}
	})

	t.Run("should exit permission to DELEGATING (green) when interrupted with teammates still in flight (case 11/Q3)", func(t *testing.T) {
		m := permMap(writeTranscript(t, tInterrupt("2026-06-01T21:39:30Z")), since)
		m[100].Claude.InFlightSubagents = 1
		selfHealStaleAttention(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "delegating" {
			t.Errorf("status = %q, want delegating (interrupt but work continues)", got)
		}
	})

	t.Run("should keep permission when only tool_results land after the prompt (concurrent subagent/parallel work)", func(t *testing.T) {
		// The reported false positive: a teammate/subagent (or a sibling auto-tool)
		// keeps flushing tool_results dated after the prompt while it is still
		// genuinely pending. None of that is the user's decision, so the chip must
		// stay red — even long after, since pending must keep nagging.
		m := permMap(writeTranscript(t, tResult("2026-06-01T21:39:30Z"), tResult("2026-06-01T21:40:00Z")), since)
		selfHealStaleAttention(m, now.Add(time.Hour), testTune, nil)
		if got := m[100].Claude.Status; got != "permission" {
			t.Errorf("status = %q, want permission (concurrent work is not a decision)", got)
		}
	})

	t.Run("should keep permission when the newest resolution entry predates the prompt (unflushed pending)", func(t *testing.T) {
		// The pending prompt's tool_use is not flushed; the tail shows only the
		// previous assistant turn, dated before the chip went red. This is the
		// nginx-template-setup over-demotion regression.
		m := permMap(writeTranscript(t, tAssistant("2026-06-01T21:36:45Z")), since)
		selfHealStaleAttention(m, now.Add(time.Hour), testTune, nil) // even long after
		if got := m[100].Claude.Status; got != "permission" {
			t.Errorf("status = %q, want permission (pending must keep nagging)", got)
		}
	})

	t.Run("should demote when the transcript is unreadable and the ttl elapsed", func(t *testing.T) {
		m := permMap("/no/such/transcript.jsonl", now.Add(-2*testTune.PermissionDecayTTL))
		selfHealStaleAttention(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (fail-soft backstop)", got)
		}
	})

	t.Run("should keep permission when the transcript is unreadable but within the ttl", func(t *testing.T) {
		m := permMap("", now) // empty path, fresh
		selfHealStaleAttention(m, now, testTune, nil)
		if got := m[100].Claude.Status; got != "permission" {
			t.Errorf("status = %q, want permission (too soon to give up)", got)
		}
	})

	t.Run("should leave non-permission sessions untouched", func(t *testing.T) {
		m := map[int]*state.Session{
			1: {PID: 1, Claude: &state.ClaudeInfo{Status: "working"}},
			2: {PID: 2, Claude: &state.ClaudeInfo{Status: "idle"}},
			3: {PID: 3}, // no Claude block at all
		}
		selfHealStaleAttention(m, now, testTune, nil)
		if got := m[1].Claude.Status; got != "working" {
			t.Errorf("working session changed to %q", got)
		}
		if got := m[2].Claude.Status; got != "idle" {
			t.Errorf("idle session changed to %q", got)
		}
	})
}
