// Package barview prepares daemon sessions for status-bar adapters.
//
// It deliberately stops short of choosing a wire format or layout: Waybar,
// Polybar, a TUI, and a future macOS bar have different presentation surfaces.
// They do, however, need one definition of status, flags, and action identity.
package barview

import (
	"fmt"
	"strings"

	"github.com/tjmisko/switchboard/internal/state"
)

// Chip is the bar-neutral part of one rendered session. Session remains
// available to adapters for surface-specific detail such as a Waybar tooltip.
type Chip struct {
	Session  state.Session
	Status   string
	Classes  []string
	Selector string
}

// Prepare returns at most limit chips in snapshot order. A non-positive limit
// means unlimited; adapters may represent the omitted count however they like.
func Prepare(snapshot state.Snapshot, limit int) []Chip {
	count := len(snapshot.Sessions)
	if limit > 0 && count > limit {
		count = limit
	}
	chips := make([]Chip, count)
	for i := range count {
		session := snapshot.Sessions[i]
		status := Status(session)
		classes := []string{ColorClass(status)}
		if status == state.StatusDelegating {
			classes = append(classes, "delegating")
		}
		if session.Focused {
			classes = append(classes, "focused")
		}
		if session.Suspended {
			classes = append(classes, "suspended")
		}
		if session.Headless {
			classes = append(classes, "headless")
		}
		if session.Remote {
			classes = append(classes, "remote")
		}
		if session.Hostname != "" && !session.Navigable {
			classes = append(classes, "unnavigable")
		}
		chips[i] = Chip{
			Session:  session,
			Status:   status,
			Classes:  classes,
			Selector: FocusSelector(session),
		}
	}
	return chips
}

// Status resolves the provider-neutral effective status. AgentGraph is the
// fallback for snapshots whose compatibility enrichment has not been filled.
func Status(session state.Session) string {
	if info := session.Enrichment(); info != nil && info.Status != "" {
		return info.Status
	}
	if session.AgentGraph != nil && session.AgentGraph.Summary.Status != "" {
		return session.AgentGraph.Summary.Status
	}
	return "unknown"
}

// ColorClass maps delegating onto working while retaining delegating as a
// secondary class in Prepare. This keeps the existing green/no-attention rule.
func ColorClass(status string) string {
	if status == state.StatusDelegating {
		return state.StatusWorking
	}
	return status
}

// FocusSelector returns the stable switchboard-ctl selector for a chip, or an
// empty string when the session cannot be navigated from this machine.
func FocusSelector(session state.Session) string {
	if session.PID <= 0 || session.Headless {
		return ""
	}
	if session.Remote || session.Hostname != "" {
		if !session.Navigable || strings.TrimSpace(session.Hostname) == "" {
			return ""
		}
		return fmt.Sprintf("host:%s:pid:%d", session.Hostname, session.PID)
	}
	return fmt.Sprintf("pid:%d", session.PID)
}
