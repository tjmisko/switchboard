// Package pi reads Pi's own on-disk evidence. Pi's live status comes from its
// extension's hooks (cmd/switchboard/pi_hooks.go); this package covers the gap
// a daemon restart leaves before the next hook arrives.
package pi

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/tailcache"
	"github.com/tjmisko/switchboard/internal/tailread"
)

// sessionTailBytes bounds the read, as the Codex rollout read is bounded: one
// assistant message plus its tool results fits comfortably, and a long session
// is never scanned from the top.
const sessionTailBytes = 256 * 1024

// SessionTail is what a Pi session file's tail says about the run: Runtime is
// active when the newest message on the active branch is the user's (Pi
// appends it as a run starts) or an assistant message that stopped to call a
// tool, idle when that assistant message stopped for any other reason, and
// unknown when the tail holds neither on that branch. At is that message's
// timestamp.
type SessionTail struct {
	Runtime agentgraph.RuntimeState
	At      time.Time
}

// sessionEntry decodes only the fields that place an entry in the tree and
// classify an assistant message. Message content is never retained.
type sessionEntry struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	ParentID  *string   `json:"parentId"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Role       string `json:"role"`
		StopReason string `json:"stopReason"`
	} `json:"message"`
}

// ReadSessionTail reads the bounded tail of a Pi session file
// (~/.pi/agent/sessions/--<path>--/<ts>_<uuid>.jsonl).
//
// Entries form a tree by id/parentId, and Pi takes the file's last entry as
// the leaf when it loads a session, so the active branch is the parent chain
// from the last entry. A later line can belong to an abandoned branch only
// until the next append, which always extends the leaf. The walk stops at the
// first user or assistant message; when the chain leaves the window first, or
// the newest row is still being written, the tail is no evidence.
//
// The answer is cached by the file's identity (#98): an unchanged session file
// is stat'ed, not re-read.
func ReadSessionTail(path string) (SessionTail, error) {
	if path == "" {
		return readSessionTail(path)
	}
	return tailcache.Load(tailcache.Default(), path, "pi/session-tail", readSessionTail)
}

func readSessionTail(path string) (SessionTail, error) {
	unknown := SessionTail{Runtime: agentgraph.RuntimeUnknown}
	data, complete, err := tailread.Lines(path, sessionTailBytes)
	if err != nil || !complete {
		return unknown, err
	}
	entries := make(map[string]sessionEntry)
	leaf := ""
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var entry sessionEntry
		if json.Unmarshal(line, &entry) != nil || entry.ID == "" || entry.Type == "session" {
			continue
		}
		entries[entry.ID] = entry
		leaf = entry.ID
	}
	// len(entries) bounds the walk, so a malformed cycle cannot spin.
	for id, steps := leaf, 0; id != "" && steps <= len(entries); steps++ {
		entry, ok := entries[id]
		if !ok {
			break
		}
		if entry.Type == "message" && entry.Message.Role == "user" {
			return SessionTail{Runtime: agentgraph.RuntimeActive, At: entry.Timestamp}, nil
		}
		if entry.Type == "message" && entry.Message.Role == "assistant" {
			if entry.Message.StopReason == "toolUse" {
				return SessionTail{Runtime: agentgraph.RuntimeActive, At: entry.Timestamp}, nil
			}
			return SessionTail{Runtime: agentgraph.RuntimeIdle, At: entry.Timestamp}, nil
		}
		if entry.ParentID == nil {
			break
		}
		id = *entry.ParentID
	}
	return unknown, nil
}
