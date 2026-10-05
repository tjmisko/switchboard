package waybarchip

import (
	"slices"
	"strings"
	"testing"
	"time"

	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestRenderSlotShouldGreyALimitedChipAndNameItsReset(t *testing.T) {
	resetsAt := time.Now().Add(time.Hour)
	snap := state.Snapshot{Sessions: []state.Session{{
		PID: 4821, CWD: "/home/u/proj", Agent: state.AgentKindCodex,
		Codex:      &state.AgentInfo{Status: state.StatusLimited},
		UsageLimit: &state.UsageLimit{ObservedAt: time.Now(), ResetsAt: &resetsAt},
	}}}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, "unknown") || !slices.Contains(out.Class, "limited") {
		t.Fatalf("classes = %v, want unknown + limited", out.Class)
	}
	if out.Alt != "unknown" {
		t.Fatalf("alt = %q, want unknown", out.Alt)
	}
	if !strings.Contains(out.Tooltip, "usage limit · resets") || !strings.Contains(out.Tooltip, "#6c7086") {
		t.Fatalf("tooltip should show a grey dot and the reset: %q", out.Tooltip)
	}
}
