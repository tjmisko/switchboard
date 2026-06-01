package main

import (
	"os"
	"path/filepath"
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
