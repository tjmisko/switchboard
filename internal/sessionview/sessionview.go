// Package sessionview prepares the ordered session list for every consumer.
//
// It deliberately stops short of choosing a wire format or layout: Waybar,
// Polybar, a TUI, and a future macOS bar have different presentation surfaces.
// They do, however, need one definition of status, flags, and action identity.
package sessionview

import (
	"fmt"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// Item is the display-independent view of one session. Session remains
// available to adapters for surface-specific detail such as a Waybar tooltip.
type Item struct {
	Session  state.Session
	Status   string
	Classes  []string
	Selector string
}

// Prepare returns at most limit items in snapshot order. A non-positive limit
// means unlimited; adapters may represent the omitted count however they like.
func Prepare(snapshot state.Snapshot, limit int) []Item {
	count := len(snapshot.Sessions)
	if limit > 0 && count > limit {
		count = limit
	}
	items := make([]Item, count)
	for i := range count {
		session := snapshot.Sessions[i]
		status := Status(session)
		classes := []string{ColorClass(status)}
		if status == state.StatusDelegating {
			classes = append(classes, "delegating")
		}
		if status == state.StatusLimited {
			classes = append(classes, "limited")
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
		items[i] = Item{
			Session:  session,
			Status:   status,
			Classes:  classes,
			Selector: FocusSelector(session),
		}
	}
	return items
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

// ColorClass maps delegating onto working and limited onto unknown, retaining
// each as a secondary class in Prepare. Delegating keeps the green/no-attention
// rule; limited paints grey in every stylesheet that already greys unknown.
func ColorClass(status string) string {
	switch status {
	case state.StatusDelegating:
		return state.StatusWorking
	case state.StatusLimited:
		return "unknown"
	}
	return status
}

// LimitDetail spells out a limited session's status for a tooltip: "usage
// limit · resets 5:15 PM", with the weekday when the reset is not today, and
// plain "usage limit" when the provider named no reset.
func LimitDetail(session state.Session, now time.Time) string {
	if session.UsageLimit == nil || session.UsageLimit.ResetsAt == nil {
		return "usage limit"
	}
	resetsAt := session.UsageLimit.ResetsAt.In(now.Location())
	layout := "3:04 PM"
	if y, m, d := resetsAt.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
		layout = "Mon 3:04 PM"
	}
	return "usage limit · resets " + resetsAt.Format(layout)
}

// FocusSelector returns the stable switchboard-ctl selector for a session, or an
// empty string when the session cannot be navigated from this machine.
func FocusSelector(session state.Session) string {
	if session.PID <= 0 || session.Headless {
		return ""
	}
	if session.Remote || session.Hostname != "" {
		if !session.Navigable || strings.TrimSpace(session.Hostname) == "" {
			return ""
		}
		selector := fmt.Sprintf("host:%s:pid:%d", session.Hostname, session.PID)
		if !session.StartedAt.IsZero() {
			selector += ":started:" + session.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		return selector
	}
	return fmt.Sprintf("pid:%d", session.PID)
}
