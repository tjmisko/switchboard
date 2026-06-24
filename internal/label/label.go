// Package label centralizes how a session is named for display. It sources the
// raw human name (preferring the authoritative Claude session name on disk),
// then applies project prefixing via internal/projectname so the bottom-bar
// chips and `switchboard-ctl` list/pick agree on a single label.
package label

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
)

// spinnerPrefixes are the leading glyphs Claude Code writes onto the wezterm
// window title while working; we strip them so the bare name shows.
var spinnerPrefixes = []string{"✳ ", "⠂ ", "⠐ ", "⠁ ", "⠈ ", "⠠ ", "⠄ ", "⡀ ", "⢀ "}

// RawName picks the human name for a session before project prefixing:
//  1. the Claude session name from ~/.claude/sessions/<pid>.json (what `/name`
//     and the launcher both set — authoritative and terminal-independent);
//  2. the wezterm window title with any spinner glyph stripped;
//  3. the cwd basename;
//  4. "pid N" as a last resort.
func RawName(s state.Session) string {
	if n := claudeSessionName(s.PID); n != "" {
		return n
	}
	if s.Wezterm != nil && s.Wezterm.WindowTitle != "" {
		title := s.Wezterm.WindowTitle
		for _, p := range spinnerPrefixes {
			if rest, ok := strings.CutPrefix(title, p); ok {
				title = rest
				break
			}
		}
		return title
	}
	if s.CWD != "" {
		return filepath.Base(s.CWD)
	}
	return fmt.Sprintf("pid %d", s.PID)
}

// Chip returns the project-prefixed, de-duplicated label for a session's chip.
func Chip(cfg projectname.Config, s state.Session) string {
	return projectname.ResolveForDir(cfg, s.CWD, RawName(s))
}

// claudeSessionName reads the `name` field from ~/.claude/sessions/<pid>.json,
// returning "" when the file is absent, unreadable, or carries no name.
func claudeSessionName(pid int) string {
	home, err := os.UserHomeDir()
	if err != nil || pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", pid)))
	if err != nil {
		return ""
	}
	var rec struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &rec) != nil {
		return ""
	}
	return strings.TrimSpace(rec.Name)
}
