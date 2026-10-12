package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

type billingStatusline struct {
	SessionID     string          `json:"session_id"`
	RateLimits    json.RawMessage `json:"rate_limits"`
	ContextWindow struct {
		CurrentUsage     json.RawMessage `json:"current_usage"`
		TotalInputTokens int64           `json:"total_input_tokens"`
	} `json:"context_window"`
	Cost struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
	} `json:"cost"`
}

func statuslineEvidence(body []byte) (string, *bool) {
	var line billingStatusline
	if json.Unmarshal(body, &line) != nil || len(line.SessionID) == 0 || len(line.SessionID) > 128 {
		return "", nil
	}
	seen := len(line.ContextWindow.CurrentUsage) > 0 && string(line.ContextWindow.CurrentUsage) != "null"
	seen = seen || line.ContextWindow.TotalInputTokens > 0 || line.Cost.TotalCostUSD > 0
	if !seen {
		return "", nil
	}
	present := len(line.RateLimits) > 0 && string(line.RateLimits) != "null"
	return line.SessionID, &present
}

func billingMarker(sessionID string) string {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(runtimeDir, "switchboard", "billing-evidence", hex.EncodeToString(sum[:]))
}

// cmdBillingStatusline consumes the statusline's JSON in a separate process.
// It sends one boolean after the first response per session; no other field
// crosses RPC. A runtime marker deduplicates later statusline refreshes.
func cmdBillingStatusline(c *rpc.Client) {
	body, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	if err != nil {
		return
	}
	sessionID, present := statuslineEvidence(body)
	if present == nil {
		return
	}
	marker := billingMarker(sessionID)
	if marker != "" {
		if err := os.MkdirAll(filepath.Dir(marker), 0700); err == nil {
			f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if os.IsExist(err) {
				return
			}
			if err == nil {
				_ = f.Close()
			} else {
				marker = ""
			}
		} else {
			marker = ""
		}
	}
	req := rpc.Request{Cmd: "hook", Agent: state.AgentKindClaude, Event: "BillingEvidence", PID: os.Getppid(),
		SessionID: strings.TrimSpace(sessionID), ObservedAt: time.Now().UTC(), RateLimitsPresent: present}
	if err := c.Send(req); err != nil {
		if marker != "" {
			_ = os.Remove(marker)
		}
		return
	}
	var response rpc.Response
	if err := c.Recv(&response); (err != nil || response.Error != "") && marker != "" {
		_ = os.Remove(marker)
	}
}
