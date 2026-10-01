package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// ReadRolloutRuntime reads explicit lifecycle markers from an already-bound
// rollout. It never finds a rollout by CWD/recency and never infers idle from
// elapsed time. Oversized content rows do not prevent reading later markers.
func ReadRolloutRuntime(path string) (agentgraph.RuntimeState, time.Time, error) {
	const tailBytes = 256 * 1024
	f, err := os.Open(path)
	if err != nil {
		return agentgraph.RuntimeUnknown, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return agentgraph.RuntimeUnknown, time.Time{}, err
	}
	start := fi.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return agentgraph.RuntimeUnknown, time.Time{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, tailBytes))
	if err != nil {
		return agentgraph.RuntimeUnknown, time.Time{}, err
	}
	if start > 0 {
		if n := bytes.IndexByte(data, '\n'); n >= 0 {
			data = data[n+1:]
		} else {
			data = nil
		}
	}
	// A partially-written new row may be a start marker; defer until it flushes.
	if len(data) != 0 && data[len(data)-1] != '\n' {
		return agentgraph.RuntimeUnknown, time.Time{}, nil
	}
	runtime, newest := agentgraph.RuntimeUnknown, time.Time{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var event struct {
			Timestamp time.Time `json:"timestamp"`
			Type      string    `json:"type"`
			Payload   struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &event) != nil || event.Type != "event_msg" || !event.Timestamp.After(newest) {
			continue
		}
		var next agentgraph.RuntimeState
		switch event.Payload.Type {
		case "task_complete", "turn_complete", "turn_completed", "turn_aborted", "shutdown_complete":
			next = agentgraph.RuntimeIdle
		case "task_started", "turn_started", "agent_reasoning", "agent_message", "exec_command_begin", "mcp_tool_call_begin":
			next = agentgraph.RuntimeActive
		default:
			continue
		}
		runtime, newest = next, event.Timestamp
	}
	return runtime, newest, nil
}
