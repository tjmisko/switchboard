// Command switchboard-polybar subscribes to the Switchboard daemon and emits
// one Polybar-formatted line per snapshot. It is intended for a custom/script
// module with tail=true: one long-lived process renders every session, while
// inline action tags make each navigable chip independently clickable.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

const reconnectDelay = 2 * time.Second

type palette struct {
	working    string
	idle       string
	permission string
	unknown    string
}

type renderOptions struct {
	maxSessions int
	ctlPath     string
	colors      palette
}

func main() {
	options := renderOptions{colors: palette{}}
	socketPath := flag.String("socket", defaultSocketPath(), "daemon socket")
	flag.IntVar(&options.maxSessions, "max-sessions", 10, "maximum session chips; 0 means unlimited")
	flag.StringVar(&options.ctlPath, "ctl", defaultCTLPath(), "path to switchboard-ctl for chip actions")
	flag.StringVar(&options.colors.working, "working-color", "#8ABEB7", "working/delegating chip color")
	flag.StringVar(&options.colors.idle, "idle-color", "#F0C674", "idle chip color")
	flag.StringVar(&options.colors.permission, "permission-color", "#A54242", "permission chip color")
	flag.StringVar(&options.colors.unknown, "unknown-color", "#707880", "unknown, overflow, and disconnected color")
	flag.Parse()

	if options.maxSessions < 0 {
		fmt.Fprintln(os.Stderr, "switchboard-polybar: -max-sessions must be non-negative")
		os.Exit(2)
	}
	for name, color := range map[string]string{
		"working": options.colors.working, "idle": options.colors.idle,
		"permission": options.colors.permission, "unknown": options.colors.unknown,
	} {
		if !validColor(color) {
			fmt.Fprintf(os.Stderr, "switchboard-polybar: -%s-color must be #RGB, #RRGGBB, or #AARRGGBB\n", name)
			os.Exit(2)
		}
	}

	renderer := &liveRenderer{
		options: options,
		labels:  &sblabel.NameCache{},
	}
	out := &emitter{w: os.Stdout}
	for {
		runOnce(*socketPath, renderer, out)
		out.emit(renderUnavailable(options.colors))
		time.Sleep(reconnectDelay)
	}
}

// runOnce consumes one daemon connection. Returning is an ordinary condition:
// main emits a stable degraded chip and retries without exiting, which prevents
// a Polybar tail=true module from entering a rapid child-respawn loop.
func runOnce(socketPath string, renderer *liveRenderer, out *emitter) {
	c, err := rpc.Dial(socketPath)
	if err != nil {
		return
	}
	defer c.Close()
	if err := c.Send(rpc.Request{Cmd: "subscribe"}); err != nil {
		return
	}

	// A successful subscription may follow either a daemon or Polybar restart.
	// Force its first line through even if it matches the last rendered state.
	out.forget()
	for {
		var resp rpc.Response
		if err := c.Recv(&resp); err != nil {
			return
		}
		if resp.Snapshot != nil {
			out.emit(renderer.render(*resp.Snapshot))
		}
	}
}

// liveRenderer owns only the filesystem-backed naming caches. renderSnapshot
// below remains a pure snapshot+labels+options transformation for direct tests.
type liveRenderer struct {
	options renderOptions
	names   nameConfig
	labels  *sblabel.NameCache
}

func (r *liveRenderer) render(snap state.Snapshot) string {
	limit := len(snap.Sessions)
	if r.options.maxSessions > 0 && limit > r.options.maxSessions {
		limit = r.options.maxSessions
	}
	cfg := r.names.config()
	labels := make([]string, limit)
	for i := range limit {
		labels[i] = r.labels.Chip(cfg, snap.Sessions[i])
	}
	return renderSnapshot(snap, labels, r.options)
}

// renderSnapshot returns one complete custom/script line. Each non-headless
// session is wrapped in its own left-click action; right-click and scroll are
// module-level bindings in the Polybar configuration.
func renderSnapshot(snap state.Snapshot, labels []string, options renderOptions) string {
	if len(snap.Sessions) == 0 {
		return ""
	}

	limit := len(snap.Sessions)
	if options.maxSessions > 0 && limit > options.maxSessions {
		limit = options.maxSessions
	}
	parts := make([]string, 0, limit+1)
	for i := range limit {
		label := fmt.Sprintf("pid %d", snap.Sessions[i].PID)
		if i < len(labels) && strings.TrimSpace(labels[i]) != "" {
			label = labels[i]
		}
		parts = append(parts, renderSession(snap.Sessions[i], label, options))
	}
	if hidden := len(snap.Sessions) - limit; hidden > 0 {
		parts = append(parts, colorize(options.colors.unknown, fmt.Sprintf("+%d", hidden)))
	}
	return strings.Join(parts, "  ")
}

func renderSession(session state.Session, label string, options renderOptions) string {
	status := sessionStatus(session)
	color := statusColor(status, options.colors)
	if session.Suspended || session.Headless {
		color = options.colors.unknown
	}
	chip := colorize(color, "● "+escapeText(label))
	if session.Headless || session.PID <= 0 {
		return chip
	}
	command := shellQuote(options.ctlPath) + " focus pid:" + fmt.Sprint(session.PID)
	return "%{A1:" + command + ":}" + chip + "%{A}"
}

func renderUnavailable(colors palette) string {
	return colorize(colors.unknown, "✕")
}

func colorize(color, text string) string {
	return "%{F" + color + "}" + text + "%{F-}"
}

func sessionStatus(session state.Session) string {
	if info := session.Enrichment(); info != nil && info.Status != "" {
		return info.Status
	}
	if session.AgentGraph != nil && session.AgentGraph.Summary.Status != "" {
		return session.AgentGraph.Summary.Status
	}
	return "unknown"
}

func statusColor(status string, colors palette) string {
	switch status {
	case state.StatusPermission:
		return colors.permission
	case state.StatusIdle:
		return colors.idle
	case state.StatusWorking, state.StatusDelegating:
		return colors.working
	default:
		return colors.unknown
	}
}

// escapeText prevents a session name from opening a Polybar formatting tag or
// breaking the line-oriented custom/script protocol. The fullwidth percent is
// visually recognizable while being inert to Polybar's "%{" parser.
func escapeText(text string) string {
	text = strings.NewReplacer("\r", " ", "\n", " ", "%", "％").Replace(text)
	return strings.Join(strings.Fields(text), " ")
}

// shellQuote quotes the configured ctl path for the /bin/sh command Polybar
// runs when an action is clicked.
func shellQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", "'\"'\"'") + "'"
}

func validColor(color string) bool {
	if len(color) != 4 && len(color) != 7 && len(color) != 9 {
		return false
	}
	if color[0] != '#' {
		return false
	}
	for _, r := range color[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// nameConfig memoizes projectname.Load across daemon snapshots while noticing
// atomic config-file rewrites made by the name-management commands.
type nameConfig struct {
	loaded  bool
	path    string
	modTime time.Time
	size    int64
	cfg     projectname.Config
}

func (n *nameConfig) config() projectname.Config {
	path := projectname.ConfigPath()
	var modTime time.Time
	var size int64
	if info, err := os.Stat(path); err == nil {
		modTime, size = info.ModTime(), info.Size()
	}
	if n.loaded && n.path == path && n.size == size && n.modTime.Equal(modTime) {
		return n.cfg
	}
	n.loaded, n.path, n.modTime, n.size = true, path, modTime, size
	n.cfg = projectname.Load()
	return n.cfg
}

// emitter avoids asking Polybar to relayout a module when the rendered line is
// byte-identical to the one it already displays.
type emitter struct {
	w       io.Writer
	last    string
	hasLast bool
}

func (e *emitter) emit(line string) bool {
	if e.hasLast && e.last == line {
		return false
	}
	e.last, e.hasLast = line, true
	fmt.Fprintln(e.w, line)
	return true
}

func (e *emitter) forget() {
	e.last, e.hasLast = "", false
}

func defaultCTLPath() string {
	if path, err := exec.LookPath("switchboard-ctl"); err == nil {
		return path
	}
	if executable, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(executable), "switchboard-ctl")
	}
	return "switchboard-ctl"
}

func defaultSocketPath() string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "switchboard.sock")
	}
	return fmt.Sprintf("/tmp/switchboard-%d.sock", os.Getuid())
}
