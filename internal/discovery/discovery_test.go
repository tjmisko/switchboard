package discovery

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/proc"
)

// §2.1 IsClaude — seed table (0.3 owns the full coverage). The Scanner
// seen-set state machine (§2.2) gets its harness consumer in §0.5 once the
// injectable procSource lands; this seed exercises the pure predicate only.
func TestIsClaude(t *testing.T) {
	tests := []struct {
		name string
		info proc.Info
		want bool
	}{
		{"wrong comm", proc.Info{Comm: "bash", Exe: "/usr/bin/bash"}, false},
		{"comm match, exe masked", proc.Info{Comm: "claude", Exe: ""}, true},
		{"comm match, exe under /claude/", proc.Info{Comm: "claude", Exe: "/home/u/.local/share/claude/claude"}, true},
		{"comm match, exe elsewhere", proc.Info{Comm: "claude", Exe: "/usr/bin/claude-impostor"}, false},
		{"case sensitive comm", proc.Info{Comm: "Claude", Exe: ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsClaude(tt.info); got != tt.want {
				t.Errorf("IsClaude(%+v) = %v, want %v", tt.info, got, tt.want)
			}
		})
	}
}
