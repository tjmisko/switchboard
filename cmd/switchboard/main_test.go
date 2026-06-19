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
	"github.com/tjmisko/switchboard/internal/transcript"
)

func TestShouldDecayPermission(t *testing.T) {
	const ttl = 30 * time.Second
	tests := []struct {
		name string
		st   transcript.PromptState
		age  time.Duration
		want bool
	}{
		{"should decay when the prompt resolved, regardless of age", transcript.StateResolved, 0, true},
		{"should keep red when the prompt is still pending, regardless of age", transcript.StatePending, time.Hour, false},
		{"should keep red when inconclusive and within the ttl", transcript.StateUnknown, ttl - time.Second, false},
		{"should decay when inconclusive and past the ttl", transcript.StateUnknown, ttl + time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDecayPermission(tt.st, tt.age, ttl); got != tt.want {
				t.Errorf("shouldDecayPermission(%v, %v) = %v, want %v", tt.st, tt.age, got, tt.want)
			}
		})
	}
}

func TestDecayReason(t *testing.T) {
	if got := decayReason(transcript.StateResolved); got != "resolved" {
		t.Errorf("decayReason(resolved) = %q, want resolved", got)
	}
	if got := decayReason(transcript.StateUnknown); got != "ttl" {
		t.Errorf("decayReason(unknown) = %q, want ttl", got)
	}
	if got := decayReason(transcript.StatePending); got != "ttl" {
		t.Errorf("decayReason(pending) = %q, want ttl", got)
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

	m := permMap(writeTranscript(t, tResult("2026-06-01T21:39:30Z")), since)
	m[100].Claude.SessionID = "ce13c0f2-aaaa"
	selfHealStaleAttention(m, now, permissionDecayTTL)
	if !strings.Contains(buf.String(), "status: pid=100 session=ce13c0f2 decay permission->idle (reason=resolved") {
		t.Errorf("missing decay log line in:\n%s", buf.String())
	}

	// A still-pending chip is not demoted, so nothing is logged.
	buf.Reset()
	m = permMap(writeTranscript(t, tResult("2026-06-01T21:36:45Z")), since)
	selfHealStaleAttention(m, now, permissionDecayTTL)
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
		selfHealStuckStatus(m, now)
		if got := m[100].Claude.Status; got != "working" {
			t.Fatalf("status = %q, want working", got)
		}
		if !m[100].Claude.StatusSince.Equal(now) {
			t.Errorf("StatusSince = %v, want now %v", m[100].Claude.StatusSince, now)
		}
		if !strings.Contains(buf.String(), "status: pid=100 session=ce13c0f2 idle->working (reason=transcript-activity)") {
			t.Errorf("missing idle->working log in:\n%s", buf.String())
		}
	})

	t.Run("should keep idle when the newest activity predates the chip", func(t *testing.T) {
		m := stuckMap("idle", writeTranscript(t, tAssistant("2026-06-01T21:38:00Z")), since)
		selfHealStuckStatus(m, now)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (no fresh activity)", got)
		}
	})

	t.Run("should keep idle when only timestamp-less metadata was written", func(t *testing.T) {
		// The metadata entry bumps the file mtime (so the read happens) but carries
		// no timestamp; the newest conversational entry predates the chip, so it
		// must not be mistaken for fresh activity.
		m := stuckMap("idle", writeTranscript(t, tAssistant("2026-06-01T21:38:00Z"), tMeta), since)
		selfHealStuckStatus(m, now)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (metadata is not activity)", got)
		}
	})

	t.Run("should flip working to idle when the turn was interrupted", func(t *testing.T) {
		var buf bytes.Buffer
		defer log.SetOutput(log.Writer())
		log.SetOutput(&buf)

		m := stuckMap("working", writeTranscript(t, tInterrupt("2026-06-01T21:39:30Z")), since)
		selfHealStuckStatus(m, now)
		if got := m[100].Claude.Status; got != "idle" {
			t.Fatalf("status = %q, want idle", got)
		}
		if !strings.Contains(buf.String(), "status: pid=100 session=ce13c0f2 working->idle (reason=interrupt)") {
			t.Errorf("missing working->idle log in:\n%s", buf.String())
		}
	})

	t.Run("should keep working during ongoing activity with no interrupt", func(t *testing.T) {
		// A long tool run / active turn writes activity, never the interrupt marker,
		// so a busy session is never falsely decayed.
		m := stuckMap("working", writeTranscript(t, tAssistant("2026-06-01T21:39:30Z"), tResult("2026-06-01T21:39:40Z")), since)
		selfHealStuckStatus(m, now)
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
		selfHealStuckStatus(m, now)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (mtime pre-gate skips the read)", got)
		}
	})

	t.Run("should leave a permission session untouched", func(t *testing.T) {
		m := stuckMap("permission", writeTranscript(t, tResult("2026-06-01T21:39:30Z")), since)
		selfHealStuckStatus(m, now)
		if got := m[100].Claude.Status; got != "permission" {
			t.Errorf("status = %q, want permission (owned by selfHealStaleAttention)", got)
		}
	})

	t.Run("should not re-flip a session decayed permission->idle in the same tick", func(t *testing.T) {
		// Mirrors reconcileOnce ordering: a declined permission decays to idle with
		// StatusSince=now, and the resolving tool_result (older than now) must not
		// then be read as fresh activity by selfHealStuckStatus.
		m := permMap(writeTranscript(t, tResult("2026-06-01T21:39:30Z")), since)
		selfHealStaleAttention(m, now, permissionDecayTTL)
		selfHealStuckStatus(m, now)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (no immediate re-flip to working)", got)
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

	t.Run("should demote permission to idle when a tool_result lands after the prompt (declined)", func(t *testing.T) {
		m := permMap(writeTranscript(t, tResult("2026-06-01T21:39:30Z")), since)
		selfHealStaleAttention(m, now, permissionDecayTTL)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle", got)
		}
	})

	t.Run("should keep permission when the newest tool_result predates the prompt (unflushed pending)", func(t *testing.T) {
		// The pending prompt's tool_use is not flushed; the tail shows only the
		// previous resolved tool, dated before the chip went red. This is the
		// nginx-template-setup over-demotion regression.
		m := permMap(writeTranscript(t, tResult("2026-06-01T21:36:45Z")), since)
		selfHealStaleAttention(m, now.Add(time.Hour), permissionDecayTTL) // even long after
		if got := m[100].Claude.Status; got != "permission" {
			t.Errorf("status = %q, want permission (pending must keep nagging)", got)
		}
	})

	t.Run("should demote when the transcript is unreadable and the ttl elapsed", func(t *testing.T) {
		m := permMap("/no/such/transcript.jsonl", now.Add(-2*permissionDecayTTL))
		selfHealStaleAttention(m, now, permissionDecayTTL)
		if got := m[100].Claude.Status; got != "idle" {
			t.Errorf("status = %q, want idle (fail-soft backstop)", got)
		}
	})

	t.Run("should keep permission when the transcript is unreadable but within the ttl", func(t *testing.T) {
		m := permMap("", now) // empty path, fresh
		selfHealStaleAttention(m, now, permissionDecayTTL)
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
		selfHealStaleAttention(m, now, permissionDecayTTL)
		if got := m[1].Claude.Status; got != "working" {
			t.Errorf("working session changed to %q", got)
		}
		if got := m[2].Claude.Status; got != "idle" {
			t.Errorf("idle session changed to %q", got)
		}
	})
}
