// Command switchboard-waybar subscribes to the daemon and emits waybar JSON. It
// runs in one of two modes:
//
//	(no flag)        aggregate mode — emits a single chip-set in one module
//	                 (kept for debugging; not used by the live bar)
//	--slot N         slot mode — emits only the Nth session's chip, or an
//	                 empty class if no session exists at that index. Used by
//	                 ~/.config/waybar/config.jsonc, which declares N slot
//	                 modules so each can carry real GTK CSS (border, hover,
//	                 padding).
//
// Waybar's row does not wrap, so when many sessions crowd the bar each slot
// abbreviates its label with an ellipsis to fit (internal/barlayout.Fit). Every
// slot sees the full snapshot and computes the same fit, so the abbreviation is
// consistent across chips; the tooltip still shows the full, untruncated name.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/barlayout"
	"github.com/tjmisko/switchboard/internal/durfmt"
	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

type waybarOutput struct {
	Text    string   `json:"text"`
	Tooltip string   `json:"tooltip,omitempty"`
	Class   []string `json:"class"`
	Alt     string   `json:"alt,omitempty"`
}

const maxTooltipAgentRows = 6

func main() {
	socketPath := flag.String("socket", defaultSocketPath(), "daemon socket")
	slot := flag.Int("slot", -1, "emit JSON for Nth session only (waybar slot mode)")
	widthPx := flag.Float64("width-px", 0, "usable bar width in px; 0 auto-detects via hyprctl")
	flag.Parse()

	// The abbreviation budget is the monitor's usable width against fixed chip
	// metrics. Width is stable for the bar's lifetime, so resolve it once.
	availPx := resolveAvailPx(*widthPx)
	metrics := barlayout.DefaultMetrics()

	names := &nameConfig{}
	// One cache for the process lifetime: every emission names every session in
	// the snapshot, and the files behind those names barely ever move.
	labels := &sblabel.NameCache{}
	out := &emitter{w: os.Stdout}

	for {
		runOnce(*socketPath, *slot, availPx, metrics, names, labels, out)
		// Daemon socket dropped — emit a degraded chip so waybar shows
		// something while we wait, then retry.
		if *slot >= 0 {
			out.emit(waybarOutput{Text: "", Class: []string{"empty"}})
		} else {
			out.emit(waybarOutput{Text: "✕", Tooltip: "switchboard not running", Class: []string{"tracker-down"}})
		}
		time.Sleep(2 * time.Second)
	}
}

// resolveAvailPx returns the abbreviation budget: the --width-px override when
// the bar config supplies one, otherwise the auto-detected monitor width.
//
// The override exists because barlayout.ScreenWidthPx auto-detects by forking
// `hyprctl monitors -j`, and the bar declares TEN slot modules — so every bottom
// bar spawn is ten hyprctl processes, all racing to read the same unchanging
// number. It is once per process lifetime, so this is a startup cost, not a
// per-emission one; passing the width from ~/.config/waybar/claude.jsonc removes
// the fork entirely.
//
// The value is the USABLE width, not the raw monitor width: ScreenWidthPx nets
// off an internal safety margin that this package cannot see, so a caller
// passing a monitor's full pixel width would over-budget the row and lose the
// ellipsis. Non-positive means "auto-detect", which stays the default so the bar
// keeps working with no config change.
func resolveAvailPx(widthPx float64) float64 {
	if widthPx > 0 {
		return widthPx
	}
	return barlayout.ScreenWidthPx()
}

func runOnce(socketPath string, slot int, availPx float64, metrics barlayout.Metrics, names *nameConfig, labels *sblabel.NameCache, out *emitter) {
	c, err := rpc.Dial(socketPath)
	if err != nil {
		return
	}
	defer c.Close()
	if err := c.Send(rpc.Request{Cmd: "subscribe-all"}); err != nil {
		return
	}
	// A fresh subscription means the daemon restarted (or we just started), and
	// waybar may have reloaded the module under us — our belief about the line
	// the bar is currently showing is no longer trustworthy. Forget it so the
	// first chip of this stream is always printed, at a cost of exactly one
	// redundant line per reconnect. Deliberately NOT reset on a failed dial: the
	// retry loop would then re-print the degraded chip every 2s for as long as
	// the daemon is down, which is the relayout churn the dedupe exists to stop.
	out.forget()
	for {
		var resp rpc.Response
		if err := c.Recv(&resp); err != nil {
			return
		}
		if resp.Snapshot == nil {
			continue
		}
		if slot >= 0 {
			out.emit(renderSlot(*resp.Snapshot, slot, availPx, metrics, names, labels))
		} else {
			out.emit(renderAggregate(*resp.Snapshot, names, labels))
		}
	}
}

// nameConfig memoizes projectname.Load() across emissions, re-reading the file
// only when it has changed on disk.
//
// The cache lives HERE, in the one command with a hot loop, rather than inside
// projectname.Load. Nobody else would benefit: the daemon calls Load once at
// startup (cmd/switchboard/main.go) and switchboard-ctl is a one-shot process.
// The cost of putting it in Load, though, is paid by every caller — Load is a
// bare function with no synchronization, so giving it package-level state means
// any future concurrent caller races on it silently, and it would put mutable
// global state in a package whose doc promises "only ProjectRoot, ConfigPath,
// Load, SetAbbrev and SetFull touch the filesystem". It would also make
// ctl's write-then-read-back (`name set` calls SetAbbrev then Load to echo the
// result) depend on stat granularity for correctness, where today it is
// unconditionally right. Keeping the cache in the caller that needs it leaves
// every other caller's behavior exactly as it was.
//
// Invalidation is by (path, mtime, size) rather than load-once because the bar's
// middle-click binding runs ~/.config/scripts/claude-abbrev-edit, which rewrites
// this file and expects the chips to pick the rename up on the next snapshot.
// projectname.upsertEntry writes via a temp file + rename, so a rename always
// lands a fresh mtime; size rides along to catch a same-mtime rewrite on a
// coarse-granularity filesystem. path is part of the key so a caller that moves
// XDG_CONFIG_HOME mid-process (tests do) is not served the old file's config.
type nameConfig struct {
	loaded  bool
	path    string
	modTime time.Time
	size    int64
	cfg     projectname.Config
}

// config returns the merged project-name config, re-reading it only when the
// user file's identity or stamp has moved. One stat replaces a read + unmarshal
// on the overwhelming majority of emissions.
func (n *nameConfig) config() projectname.Config {
	path := projectname.ConfigPath()
	// A missing file (the common case — the user has never renamed a project)
	// stats as the zero stamp, which is itself a perfectly good cache key: the
	// "no user overrides, defaults only" result stays cached until the file is
	// created, at which point the stamp goes non-zero and we reload.
	var modTime time.Time
	var size int64
	if fi, err := os.Stat(path); err == nil {
		modTime, size = fi.ModTime(), fi.Size()
	}
	if n.loaded && n.path == path && n.size == size && n.modTime.Equal(modTime) {
		return n.cfg
	}
	n.loaded, n.path, n.modTime, n.size = true, path, modTime, size
	n.cfg = projectname.Load()
	return n.cfg
}

// renderSlot emits JSON for the Nth session. The class array carries the
// status, a "focused" flag, and a "suspended" flag so waybar CSS can paint the
// chip. Empty slots get class=["empty"] so the CSS can collapse them.
//
// The chip text is the session's label abbreviated (with an ellipsis) so the
// whole set fits the bar; the tooltip keeps the full name. Every slot fits the
// same label set, so the abbreviation agrees across chips — which is why this
// names EVERY session, not just its own, and why the name lookup behind it is
// worth caching (see sblabel.NameCache).
func renderSlot(snap state.Snapshot, slot int, availPx float64, metrics barlayout.Metrics, names *nameConfig, cache *sblabel.NameCache) waybarOutput {
	if slot >= len(snap.Sessions) {
		return waybarOutput{Text: "", Class: []string{"empty"}}
	}
	cfg := names.config()
	labels := make([]string, len(snap.Sessions))
	for i := range snap.Sessions {
		labels[i] = cache.Chip(cfg, snap.Sessions[i])
	}
	labels = barlayout.Fit(labels, availPx, metrics)
	s := snap.Sessions[slot]
	status := sessionStatus(s)
	// The primary class paints the chip's color; delegating reuses working's green
	// (Q1 default: pure green, no CSS change needed). The raw "delegating" rides
	// along as a secondary class so the bar CAN add a badge/different shade later
	// without losing the green underneath.
	classes := []string{chipClass(status)}
	if status == state.StatusDelegating {
		classes = append(classes, "delegating")
	}
	if s.Focused {
		classes = append(classes, "focused")
	}
	if s.Suspended {
		classes = append(classes, "suspended")
	}
	// Headless claude -p runs are visible but not navigable; the class lets the
	// bar CSS render them inert (no fill, muted text) so they don't read as
	// clickable chips.
	if s.Headless {
		classes = append(classes, "headless")
	}
	if s.Hostname != "" && !s.Navigable {
		classes = append(classes, "unnavigable")
	}
	return waybarOutput{
		Text:    labels[slot],
		Tooltip: sessionTooltip(cfg, cache, s, time.Now()),
		Class:   classes,
		Alt:     chipClass(status),
	}
}

// chipClass maps a session status to the CSS class that paints its color.
// delegating shares working's green; everything else maps to itself.
func chipClass(status string) string {
	if status == state.StatusDelegating {
		return state.StatusWorking
	}
	return status
}

// renderAggregate is the original single-module mode. Kept for ad-hoc
// inspection (`switchboard-waybar | jq .`) but not driven by the live bar.
func renderAggregate(snap state.Snapshot, names *nameConfig, cache *sblabel.NameCache) waybarOutput {
	if len(snap.Sessions) == 0 {
		return waybarOutput{Text: "", Tooltip: "no agent sessions", Class: []string{"empty"}}
	}
	cfg := names.config()
	var parts []string
	for _, s := range snap.Sessions {
		mark := ""
		if s.Focused {
			mark = "*"
		}
		parts = append(parts, mark+cache.Chip(cfg, s))
	}
	return waybarOutput{
		Text:  strings.Join(parts, "  "),
		Class: []string{"multi"},
		Alt:   fmt.Sprintf("%d", len(snap.Sessions)),
	}
}

func sessionStatus(s state.Session) string {
	info := s.Enrichment()
	if info != nil && info.Status != "" {
		return info.Status
	}
	if s.AgentGraph != nil && s.AgentGraph.Summary.Status != "" {
		return s.AgentGraph.Summary.Status
	}
	return "unknown"
}

// sessionTooltip renders the Compact-stacked hover with pango markup:
//
//	<b>arachne</b>   ● working
//	assess-npm-vulnerabilities
//	~/Projects/Arachne · ws 4 · pid 292511
//
// Line 1 is the project abbreviation + a status-colored dot; line 2 is the bare
// task name (the project prefix stripped, since the abbrev already shows it);
// line 3 is dimmed metadata.
func sessionTooltip(cfg projectname.Config, cache *sblabel.NameCache, s state.Session, now time.Time) string {
	var abbrev, task string
	if s.Remote {
		base := strings.TrimSpace(s.CWD)
		if base != "" {
			base = filepath.Base(filepath.Clean(base))
		}
		rule := cfg.RuleForBase(base)
		abbrev = rule.Canonical
		task = rule.StripKnownPrefix(cache.RawName(s))
	} else {
		abbrev = projectname.CanonicalForDir(cfg, s.CWD)
		task = projectname.TaskForDir(cfg, s.CWD, cache.RawName(s))
	}
	status := sessionStatus(s)
	hasAgentTree := len(barlayout.AgentRows(s.AgentGraph)) > 0

	statusText := status
	// A delegating chip is green but idle on the main thread; spell out why so the
	// green reads as "N agents working" rather than looking stuck. An ultracode
	// workflow run is the richer answer to the same question — name the workflow
	// and its progress ("workflow simplification-audit · 7/17 agents") instead of
	// the bare count, mirroring the CLI's own "N/M agents done" line.
	if !hasAgentTree && status == state.StatusDelegating {
		if wf := workflowAnnotation(s); wf != "" {
			statusText = wf
		} else if n := subagentCount(s); n > 0 {
			statusText = fmt.Sprintf("delegating · %d agent%s", n, plural(n))
		}
	}
	// A red chip says a decision is needed; with teammates running it does not say
	// WHOSE, and the user has to switch to the pane to find out — which defeats the
	// point of holding the red. Name the blocked writer(s) instead:
	//
	//	permission · escalate-cleanup · 45s
	//
	// Same shape as the delegating annotation above (detail after the status, clock
	// last). Deliberately NOT on the chip TEXT: chip labels are fitted against the
	// bar's width budget as a set (barlayout.Fit), so a label that grew when a
	// prompt appeared and shrank when it cleared would re-abbreviate every OTHER
	// chip on the row twice per prompt — and would break the stable chip identity
	// the user navigates by.
	if !hasAgentTree {
		if w := cache.BlockedWriters(s); w != "" {
			statusText += " · " + w
		}
	}
	// How long the session has held this status: "idle · 3m", "permission · 45s".
	// Skipped while suspended — the status (and its clock) is stale until resume.
	if !s.Suspended {
		if d := durfmt.Since(statusSince(s), now); d != "" {
			statusText += " · " + d
		}
	}
	if s.Suspended {
		statusText += " · suspended"
	}
	if s.Headless {
		statusText += " · headless (claude -p)"
	} else if s.Remote && !s.Navigable {
		statusText += " · observe only (pane not bound)"
	} else if s.Hostname != "" && !s.Navigable {
		statusText += " · observe only (navigation unavailable)"
	}

	ws := "-"
	if s.Hyprland != nil && s.Hyprland.Workspace != "" {
		ws = s.Hyprland.Workspace
	}
	dot := fmt.Sprintf("<span foreground='%s'>●</span>", statusColor(status))
	process := fmt.Sprintf("pid %d", s.PID)
	if s.Hostname != "" {
		process = fmt.Sprintf("%s/%d", s.Hostname, s.PID)
	}
	displayCWD := s.CWD
	if !s.Remote {
		displayCWD = contractHome(displayCWD)
	}
	meta := fmt.Sprintf("%s · ws %s · %s", displayCWD, ws, process)
	tip := fmt.Sprintf(
		"<b>%s</b>   %s %s\n%s\n<span foreground='#6c7086' size='smaller'>%s</span>",
		pangoEscape(abbrev), dot, pangoEscape(statusText),
		pangoEscape(task),
		pangoEscape(meta),
	)
	if tree := agentTooltip(s, now); tree != "" {
		tip += "\n" + tree
	}
	return tip
}

// agentTooltip renders a compact, bounded detail tree. It may prioritize rows
// for scanability, but it never re-reduces them: the chip class remains the
// daemon-projected session summary from sessionStatus.
func agentTooltip(s state.Session, now time.Time) string {
	rows := barlayout.PrioritizeAgentRows(barlayout.AgentRows(s.AgentGraph))
	rows, folded := barlayout.LimitAgentRows(rows, maxTooltipAgentRows)
	if len(rows) == 0 && folded == 0 {
		return ""
	}
	stale := s.Suspended || !s.AgentGraph.Fresh(now)
	lines := []string{"<span foreground='#6c7086' size='smaller'>agents</span>"}
	for _, row := range rows {
		stateText := barlayout.AgentStateText(row.Node)
		if stateText == "user input" {
			stateText = "question"
		}
		if stale {
			if s.Suspended {
				stateText += " · stale (root suspended)"
			} else {
				stateText += " · stale"
			}
		} else if at := barlayout.AgentStateAt(row.Node); !at.IsZero() {
			stateText += " · " + durfmt.Compact(now.Sub(at))
		}
		if usage := barlayout.AgentUsageText(row.Node); usage != "" {
			stateText += " · " + usage
		}
		prefix := strings.Repeat("  ", row.Depth) + "↳ "
		if row.Depth > 6 {
			prefix = "  " + strings.Repeat("  ", 5) + "… "
		}
		lines = append(lines, fmt.Sprintf("%s%s — %s",
			prefix, pangoEscape(barlayout.AgentName(row.Node)), pangoEscape(stateText)))
	}
	if folded > 0 {
		lines = append(lines, fmt.Sprintf("  +%d more", folded))
	}
	return strings.Join(lines, "\n")
}

// statusColor maps a session status to the pango hex color of its tooltip dot,
// matching the chip palette (working/delegating green, idle amber, permission
// red, otherwise grey).
func statusColor(status string) string {
	switch status {
	case state.StatusWorking, state.StatusDelegating:
		return "#a6e3a1"
	case state.StatusIdle:
		return "#f9e2af"
	case state.StatusPermission:
		return "#f38ba8"
	default:
		return "#6c7086"
	}
}

// contractHome replaces a leading $HOME with ~ for a shorter metadata line.
func contractHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(p, home+"/"); ok {
			return "~/" + rest
		}
	}
	return p
}

// pangoEscape escapes the pango markup metacharacters in user-controlled text
// (session/project names) so they render literally.
func pangoEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// subagentCount reports the in-flight subagent count from the session's
// enrichment block (0 when absent), for the delegating tooltip.
func subagentCount(s state.Session) int {
	if info := s.Enrichment(); info != nil {
		return info.InFlightSubagents
	}
	return 0
}

// workflowAnnotation renders the delegating detail for a session running an
// ultracode Workflow: "workflow <name> · <done>/<started> agents", the first
// active run leading and any others folded into a "+N more" (concurrent runs
// are rare and the tooltip is one line). Empty when no run is active, letting
// the caller fall back to the generic delegating count. A run whose script
// name could not be resolved shows its run id — an opaque handle beats no
// handle.
func workflowAnnotation(s state.Session) string {
	info := s.Enrichment()
	if info == nil || len(info.Workflows) == 0 {
		return ""
	}
	w := info.Workflows[0]
	name := w.Name
	if name == "" {
		name = w.RunID
	}
	text := fmt.Sprintf("workflow %s · %d/%d agents", name, w.AgentsDone, w.AgentsStarted)
	if extra := len(info.Workflows) - 1; extra > 0 {
		text += fmt.Sprintf(" (+%d more)", extra)
	}
	return text
}

// statusSince returns the wire timestamp the current status began (nil when no
// enrichment block exists or no status edge has stamped it), for the hover
// duration counter.
func statusSince(s state.Session) *time.Time {
	if info := s.Enrichment(); info != nil {
		if info.StatusSinceWire != nil {
			return info.StatusSinceWire
		}
	}
	if s.AgentGraph != nil && !s.AgentGraph.Summary.Since.IsZero() {
		return &s.AgentGraph.Summary.Since
	}
	return nil
}

// plural returns the plural suffix for n.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// emitter writes waybar JSON lines, dropping any line byte-identical to the one
// it wrote last.
//
// Waybar relayouts a module for every line it reads off the pipe, and with TEN
// slot modules subscribed the daemon's broadcast cadence buys ten relayouts per
// snapshot — most of them repainting exactly what is already on screen. Since
// the suppressed line is identical byte-for-byte, the bar's rendered state after
// a skip is by construction what it would have been after a write; the only
// thing a skip can get wrong is our BELIEF about what the bar last read, which
// is what forget() re-syncs.
//
// This is worth having despite the tooltip's live "idle · 3m" counter, which
// does make a chip's bytes change on its own as the clock advances. durfmt
// coarsens with magnitude on purpose, so the counter only ticks per-second below
// one minute and per-minute above it; measured against this machine's own
// history log, 95.6% of session-time is spent at a status age past that first
// minute, where all but one emission per minute is a duplicate.
type emitter struct {
	w    io.Writer
	last string
}

// emit writes o as a JSON line unless it repeats the previous line. It reports
// whether it wrote, for tests.
func (e *emitter) emit(o waybarOutput) bool {
	b, _ := json.Marshal(o)
	line := string(b)
	if e.last == line {
		return false
	}
	e.last = line
	fmt.Fprintln(e.w, line)
	return true
}

// forget drops the remembered line so the next emit always writes. Called on a
// fresh subscription: see the hazard note in runOnce.
func (e *emitter) forget() {
	e.last = ""
}

func defaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "switchboard.sock")
	}
	return fmt.Sprintf("/tmp/switchboard-%d.sock", os.Getuid())
}
