package label

import (
	"fmt"
	"strings"

	"github.com/tjmisko/switchboard/internal/panetitle"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
)

// Display separates presentation from Chip's legacy dashed label. Identity and
// focus selectors continue to use the session's host, PID and process lifetime.
type Display struct {
	Project string
	Task    string
}

func (d Display) String() string {
	return strings.TrimSpace(d.Project + " " + d.Task)
}

func DisplayName(cfg projectname.Config, s state.Session) Display {
	return displayName(cfg, s, claudeSessionName)
}

func (c *NameCache) DisplayName(cfg projectname.Config, s state.Session) Display {
	if c == nil {
		return DisplayName(cfg, s)
	}
	return displayName(cfg, s, c.claudeSessionName)
}

func displayName(cfg projectname.Config, s state.Session, claudeName func(int) string) Display {
	var rule projectname.ProjectRule
	if s.CWD != "" {
		if s.Remote {
			// Remote paths must never be walked on this machine.
			rule = cfg.RuleForBase(remoteProjectBase(s.CWD))
		} else {
			rule = cfg.RuleForDir(s.CWD)
		}
	}
	name := Display{Project: rule.DisplayName(), Task: rule.TaskTitle(displaySource(s, claudeName))}
	if name.String() == "" {
		// Discovery can publish a session before its cwd or task is known.
		// Keep that session visible until project metadata arrives.
		name.Project = fmt.Sprintf("pid %d", s.PID)
	}
	return name
}

// displaySource selects the unshortened task name. Provider decoration, cwd
// fallbacks and compact thread IDs are not tasks. Pi never borrows Claude's
// session file or terminal title (whose "π - directory" is only UI chrome).
func displaySource(s state.Session, claudeName func(int) string) string {
	if s.Agent == state.AgentKindPi {
		if s.Pi != nil && s.DisplayName.NativeFor(s.Pi.SessionID) {
			return s.DisplayName.Value
		}
		return ""
	}
	if s.Agent == state.AgentKindCodex {
		id := codexConversationID(s)
		if s.DisplayName.ValidFor(id) && displaySlug(s.DisplayName.Value, 1) != "" {
			return s.DisplayName.Value
		}
		if id == "" || (s.AgentGraph != nil && s.AgentGraph.RootID == id) {
			return graphRootName(s.AgentGraph)
		}
		return ""
	}
	if s.Remote {
		if s.ResolvedName != "" {
			return s.ResolvedName
		}
		return graphRootName(s.AgentGraph)
	}
	if name := claudeName(s.PID); name != "" {
		return name
	}
	if s.Wezterm != nil {
		return panetitle.Normalize(s.Wezterm.WindowTitle)
	}
	return ""
}
