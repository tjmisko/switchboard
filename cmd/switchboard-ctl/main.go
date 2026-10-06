// Command switchboard-ctl is the user-facing CLI client. It talks to the
// daemon over its Unix socket and prints either human-friendly text or raw
// JSON.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/buildinfo"
	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/sessionview"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/usagelimit"
)

func main() {
	socketPath := flag.String("socket", defaultSocketPath(), "daemon socket")
	jsonOut := flag.Bool("json", false, "emit JSON instead of human-friendly text")
	showVersion := flag.Bool("version", false, "print the build revision and exit")
	flag.Usage = usage
	flag.Parse()

	// -version answers before the argument check and before any dial: a deploy
	// script must be able to ask a staged binary what it is without a daemon
	// running, and without supplying a command it does not want to execute.
	if *showVersion {
		fmt.Println(buildinfo.Get())
		return
	}

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	// bottombar publishes the bottom Waybar modules. Its legacy `watch` mode
	// owns a split process; `publish` attaches to a combined desktop-owned
	// process. Both tolerate a down daemon and reconnect on their own.
	if args[0] == "display" {
		cmdDisplay(args[1:], *socketPath)
		return
	}
	if args[0] == "bottombar" {
		cmdBottombar(args[1:], *socketPath)
		return
	}
	// diagnose reads the journal (not the daemon socket), so it runs before the
	// mandatory dial too — it must work even when the daemon is down.
	if args[0] == "diagnose" {
		cmdDiagnose(args[1:])
		return
	}
	// name resolves/edits project abbreviations from the projectname config and
	// the filesystem only — no daemon needed, so it runs before the dial.
	if args[0] == "name" {
		cmdName(args[1:])
		return
	}
	// history reads/manages the on-disk activity log directly (like diagnose), so
	// it runs before the dial — it must work whether or not the daemon is up.
	if args[0] == "history" {
		cmdHistory(args[1:])
		return
	}
	// timeline derives swimlanes + attention stats from the on-disk activity log;
	// also file-only, so it runs before the dial.
	if args[0] == "timeline" {
		cmdTimeline(args[1:])
		return
	}
	// seed-bench measures the daemon's history-seeding cost against the on-disk
	// log (docs/seed-replay-memory-plan.md); file-only, no daemon needed.
	if args[0] == "seed-bench" {
		cmdSeedBench(args[1:])
		return
	}
	// pricing inspects or refreshes the public spot-rate cache. It needs no
	// daemon and never reads provider credentials.
	if args[0] == "pricing" {
		if err := cmdPricing(args[1:], *jsonOut, os.Stdout); err != nil {
			fail("pricing: %v", err)
		}
		return
	}

	c, err := rpc.Dial(*socketPath)
	if err != nil {
		fail("connect daemon: %v", err)
	}
	defer c.Close()

	switch args[0] {
	case "list":
		cmdList(c, *jsonOut)
	case "remote-stream":
		cmdRemoteStream(c)
	case "pane-bind":
		cmdPaneBind(c, args[1:])
	case "pane-state":
		cmdPaneState(c, args[1:])
	case "focus":
		selector := "active"
		if len(args) > 1 {
			selector = args[1]
		}
		cmdFocus(c, selector)
	case "status":
		cmdStatus(c)
	case "pick":
		cmdPick(c)
	case "cycle":
		direction := "next"
		if len(args) > 1 {
			direction = args[1]
		}
		cmdCycle(c, direction)
	case "attention":
		cmdAttention(c)
	case "agent-diagnostics":
		cmdAgentDiagnostics(c, *jsonOut)
	case "explain":
		cmdExplain(c, args[1:], *jsonOut)
	case "hook":
		if len(args) < 2 {
			fail("hook requires an event name")
		}
		cmdHook(c, args[1], state.AgentKindClaude)
	case "codex-hook":
		if len(args) < 2 {
			fail("codex-hook requires an event name")
		}
		cmdHook(c, args[1], state.AgentKindCodex)
	case "pi-hook":
		if len(args) < 2 {
			fail("pi-hook requires an event name")
		}
		cmdHook(c, args[1], state.AgentKindPi)
	case "activity":
		if len(args) < 2 {
			fail("activity requires a value: idle|active")
		}
		cmdActivity(c, args[1])
	default:
		usage()
		os.Exit(2)
	}
}

func cmdList(c *rpc.Client, jsonOut bool) {
	snap := mustList(c)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
		return
	}
	if len(snap.Sessions) == 0 {
		fmt.Println("no agent sessions")
		return
	}
	for i, s := range snap.Sessions {
		marker := " "
		if s.Focused {
			marker = "*"
		}
		identity := fmt.Sprintf("pid=%d", s.PID)
		if s.Hostname != "" {
			identity = fmt.Sprintf("host=%s pid=%d", s.Hostname, s.PID)
		}
		fmt.Printf("%s [%d] %s cwd=%s\n", marker, i, identity, s.CWD)
		if s.Wezterm != nil {
			fmt.Printf("       wezterm: mux=%d pane=%d title=%q\n", s.Wezterm.MuxPID, s.Wezterm.PaneID, s.Wezterm.WindowTitle)
		}
		if s.Hyprland != nil {
			fmt.Printf("       hypr:    addr=%s workspace=%s\n", s.Hyprland.Address, s.Hyprland.Workspace)
		}
	}
}

func cmdAgentDiagnostics(c *rpc.Client, jsonOut bool) {
	if err := c.Send(rpc.Request{Cmd: "agent-diagnostics"}); err != nil {
		fail("send: %v", err)
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		fail("recv: %v", err)
	}
	if resp.Error != "" {
		fail("%s", resp.Error)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp.Diagnostics)
		return
	}
	renderAgentDiagnostics(os.Stdout, resp.Diagnostics)
}

func renderAgentDiagnostics(w io.Writer, diagnostics []rpc.AgentDiagnostic) {
	if len(diagnostics) == 0 {
		fmt.Fprintln(w, "no agent diagnostics")
		return
	}
	for _, diagnostic := range diagnostics {
		lastAt := "-"
		if !diagnostic.LastAt.IsZero() {
			lastAt = diagnostic.LastAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s %s count=%d last_at=%s\n",
			diagnostic.Provider, diagnostic.Category, diagnostic.Count, lastAt)
	}
}

func cmdFocus(c *rpc.Client, selector string) {
	target, err := resolveFocusSelector(mustList(c).Sessions, selector)
	if err != nil {
		fail("%v", err)
	}
	focusSession(c, *target)
}

func focusSession(c *rpc.Client, target state.Session) {
	if target.Hostname == "" || target.PID <= 0 || target.StartedAt.IsZero() {
		fail("selected session has no exact federated identity")
	}
	if err := c.Send(rpc.Request{
		Cmd: "focus-session", Hostname: target.Hostname, PID: target.PID, StartedAt: target.StartedAt,
	}); err != nil {
		fail("send: %v", err)
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		fail("recv: %v", err)
	}
	if resp.Error != "" {
		fail("%s", resp.Error)
	}
}

func resolveFocusSelector(sessions []state.Session, selector string) (*state.Session, error) {
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions")
	}
	if selector == "" || selector == "active" {
		for i := range sessions {
			if sessions[i].Focused {
				return &sessions[i], nil
			}
		}
		return &sessions[0], nil
	}
	if rest, ok := strings.CutPrefix(selector, "idx:"); ok {
		index, err := strconv.Atoi(rest)
		if err != nil || index < 0 || index >= len(sessions) {
			return nil, fmt.Errorf("no session matches %q", selector)
		}
		return &sessions[index], nil
	}
	if rest, ok := strings.CutPrefix(selector, "host:"); ok {
		host, identity, ok := strings.Cut(rest, ":pid:")
		pidText := identity
		startedText := ""
		if before, after, hasStarted := strings.Cut(identity, ":started:"); hasStarted {
			pidText, startedText = before, after
		}
		pid, err := strconv.Atoi(pidText)
		if !ok || host == "" || err != nil {
			return nil, fmt.Errorf("invalid host selector %q", selector)
		}
		var startedAt time.Time
		if startedText != "" {
			startedAt, err = time.Parse(time.RFC3339Nano, startedText)
			if err != nil {
				return nil, fmt.Errorf("invalid started_at in selector %q", selector)
			}
		}
		for i := range sessions {
			if sessions[i].Hostname == host && sessions[i].PID == pid &&
				(startedAt.IsZero() || sessions[i].StartedAt.Equal(startedAt)) {
				return &sessions[i], nil
			}
		}
		return nil, fmt.Errorf("no session matches %q", selector)
	}
	if rest, ok := strings.CutPrefix(selector, "pid:"); ok {
		pid, err := strconv.Atoi(rest)
		if err != nil {
			return nil, fmt.Errorf("invalid PID selector %q", selector)
		}
		return uniquePID(sessions, pid, selector)
	}
	if host, pidText, ok := strings.Cut(selector, ":"); ok {
		pid, err := strconv.Atoi(pidText)
		if err == nil {
			return resolveFocusSelector(sessions, fmt.Sprintf("host:%s:pid:%d", host, pid))
		}
	}
	if value, err := strconv.Atoi(selector); err == nil {
		target, pidErr := uniquePID(sessions, value, selector)
		if pidErr == nil {
			return target, nil
		}
		if !errorsIsNoPID(pidErr) {
			return nil, pidErr
		}
		if value >= 0 && value < len(sessions) {
			return &sessions[value], nil
		}
	}
	return nil, fmt.Errorf("no session matches %q", selector)
}

type noPIDError struct{ selector string }

func (e noPIDError) Error() string { return fmt.Sprintf("no PID matches %q", e.selector) }
func errorsIsNoPID(err error) bool {
	_, ok := err.(noPIDError)
	return ok
}

func uniquePID(sessions []state.Session, pid int, selector string) (*state.Session, error) {
	var target *state.Session
	for i := range sessions {
		if sessions[i].PID != pid {
			continue
		}
		if target != nil {
			return nil, fmt.Errorf("PID %d is ambiguous across hosts; use host:<hostname>:pid:%d", pid, pid)
		}
		target = &sessions[i]
	}
	if target == nil {
		return nil, noPIDError{selector: selector}
	}
	return target, nil
}

func cmdPaneBind(c *rpc.Client, args []string) {
	if len(args) != 4 {
		fail("pane-bind requires payload gui-pid window-id pane-id")
	}
	guiPID := mustIntArg("gui-pid", args[1])
	windowID := mustIntArg("window-id", args[2])
	paneID := mustIntArg("pane-id", args[3])
	sendOK(c, rpc.Request{Cmd: "pane-bind", Binding: args[0], GUIPID: guiPID, WindowID: windowID, PaneID: paneID})
}

func cmdPaneState(c *rpc.Client, args []string) {
	if len(args) != 4 {
		fail("pane-state requires gui-pid window-id active-pane-id window-focused")
	}
	windowFocused, err := strconv.ParseBool(args[3])
	if err != nil {
		fail("window-focused must be true or false")
	}
	sendOK(c, rpc.Request{
		Cmd: "pane-state", GUIPID: mustIntArg("gui-pid", args[0]),
		WindowID: mustIntArg("window-id", args[1]), PaneID: mustIntArg("active-pane-id", args[2]),
		WindowFocused: windowFocused,
	})
}

func mustIntArg(name, value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		fail("%s must be an integer", name)
	}
	return n
}

func sendOK(c *rpc.Client, request rpc.Request) {
	if err := c.Send(request); err != nil {
		fail("send: %v", err)
	}
	var response rpc.Response
	if err := c.Recv(&response); err != nil {
		fail("recv: %v", err)
	}
	if response.Error != "" {
		fail("%s", response.Error)
	}
}

// cmdActivity reports a global user-activity edge to the daemon — "idle" when an
// idle daemon (e.g. hypridle) sees no input for its timeout, "active" when input
// resumes. Session-less; the daemon records it for the delegation/attention
// metrics. Mirrors cmdFocus: send, surface the daemon's reply, exit nonzero on a
// rejected value (the daemon is the single validator of idle|active).
func cmdActivity(c *rpc.Client, value string) {
	if err := c.Send(rpc.Request{Cmd: "activity", Activity: value}); err != nil {
		fail("send: %v", err)
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		fail("recv: %v", err)
	}
	if resp.Error != "" {
		fail("%s", resp.Error)
	}
}

func cmdStatus(c *rpc.Client) {
	snap := mustList(c)
	fmt.Printf("%d session(s)\n", len(snap.Sessions))
}

// cmdPick emits one tab-separated line per session, ordered as the snapshot.
// Format: EXACT-TOKEN \t LABEL \t WORKSPACE \t CWD
// LABEL is the project-prefixed, de-duplicated session name (see
// internal/label). Intended to be piped into fzf with `--with-nth=2..` so the
// user sees the label but the PID stays in the selected line for the focus call.
func cmdPick(c *rpc.Client) {
	snap := mustList(c)
	cfg := projectname.Load()
	for _, s := range snap.Sessions {
		// Headless runs (claude -p) have no window to jump to; offering them in
		// the picker would only produce a failed focus.
		if !sessionNavigable(s) {
			continue
		}
		label := sblabel.Chip(cfg, s)
		if s.Remote {
			label = s.Hostname + ":" + label
		}
		ws := "-"
		if s.Hyprland != nil && s.Hyprland.Workspace != "" {
			ws = s.Hyprland.Workspace
		}
		focusMark := " "
		if s.Focused {
			focusMark = "*"
		}
		fmt.Printf("host:%s:pid:%d:started:%s\t%s %s\tws %s\t%s\n",
			s.Hostname, s.PID, s.StartedAt.UTC().Format(time.RFC3339Nano), focusMark, label, ws, s.CWD)
	}
}

// cmdCycle focuses the next or previous session, wrapping. Position is
// determined by the focused session; if none is focused, "next" picks the
// first session and "prev" picks the last.
func cmdCycle(c *rpc.Client, direction string) {
	if direction != "next" && direction != "up" && direction != "prev" && direction != "down" {
		fail("cycle direction must be next|prev (got %q)", direction)
	}
	snap := mustList(c)
	target, ok := cycleTargetSession(snap.Sessions, direction)
	if !ok {
		return
	}
	focusSession(c, target)
}

func cycleTargetSession(sessions []state.Session, direction string) (state.Session, bool) {
	return sessionview.Cycle(sessions, direction)
}

func sessionNavigable(session state.Session) bool {
	return sessionview.Navigable(session)
}

// cycleTargetPID picks the session the cycle lands on. The ring is the
// navigable (non-headless) sessions in snapshot order — headless claude -p
// runs sit in the bar for visibility but have no window, so scrolling skips
// them. Returns false when the ring is empty.
func cycleTargetPID(sessions []state.Session, direction string) (int, bool) {
	target, ok := cycleTargetSession(sessions, direction)
	return target.PID, ok
}

// cmdAttention jumps toward the most urgent populated tier. When focus is
// already in that tier, it cycles peers there and may toggle at most one tier
// above it, so repeated presses can never climb all the way from red to green.
// Bound to mod+a in Hyprland.
func cmdAttention(c *rpc.Client) {
	target := nextAttentionTarget(mustList(c).Sessions)
	if target == nil {
		return
	}
	focusSession(c, *target)
}

// attentionRing orders the navigable sessions that `attention` may visit. It
// starts with the most urgent populated tier and includes no more than one
// adjacent colour above it:
//
//   - any red: red + orange (green is unreachable)
//   - otherwise any orange: orange + green
//   - otherwise: green
//
// This ceiling is computed from the snapshot, not from the focused session.
// Consequently repeated presses stay within the same bounded set instead of
// using an orange as a staircase from red to green. Within each tier, follow
// snapshot order to the right of focus, wrapping at the end. With no focused
// navigable session, start at the left edge. Unknown (grey) sessions are excluded —
// they are not actionable, and `cycle next|prev` already reaches every session
// regardless of colour. Headless and unbound-remote rows are excluded by
// sessionNavigable. Suspended (Ctrl-Z'd) sessions are excluded too: their
// status is frozen and they cannot act, so they neither join the ring nor bound
// it. A focused suspended session still anchors "to the right of focus", and
// `cycle next|prev` still reaches it.
func attentionRing(sessions []state.Session) []*state.Session {
	start := 0
	for i := range sessions {
		if sessionNavigable(sessions[i]) && sessions[i].Focused {
			start = i + 1
			break
		}
	}
	var permission, idle, working []*state.Session
	for step := range sessions {
		i := (start + step) % len(sessions)
		if !sessionNavigable(sessions[i]) || sessions[i].Suspended {
			continue
		}
		switch sessionStatus(sessions[i]) {
		case state.StatusPermission:
			permission = append(permission, &sessions[i])
		case state.StatusIdle:
			idle = append(idle, &sessions[i])
		case state.StatusWorking, state.StatusDelegating:
			working = append(working, &sessions[i])
		}
	}
	if len(permission) > 0 {
		return append(permission, idle...)
	}
	if len(idle) > 0 {
		return append(idle, working...)
	}
	return working
}

// nextAttentionTarget returns the session `attention` should focus. A press
// from outside the most urgent populated tier enters that tier at its next
// member to the right, wrapping at the end of the bar. A press from inside it
// advances to another un-focused peer in the same tier; only when every urgent
// member is already focused may it toggle to the adjacent tier, also to the
// right. Thus orange always jumps to red while any red exists, and an orange
// can never become a staircase to green.
//
// Skip the entire focused set for legacy terminals that report window-only
// focus. The WezTerm integration selects just one pane, so its siblings remain
// valid targets in the same window.
//
// Returns nil when the bounded ring is empty, or when every member in the
// allowed one-layer range is already focused. A less urgent session may exist
// outside that range; `cycle next|prev` remains the unrestricted navigator.
func nextAttentionTarget(sessions []state.Session) *state.Session {
	// The ring puts urgency first and orders each tier to the right of focus.
	// Only exhausted (entirely focused) urgent tiers allow a one-layer toggle.
	for _, candidate := range attentionRing(sessions) {
		if !candidate.Focused {
			return candidate
		}
	}
	return nil
}

// sessionStatus normalizes a missing or empty agent status to "unknown",
// matching switchboard-waybar's rendering.
func sessionStatus(s state.Session) string {
	info := s.Enrichment()
	if info == nil || info.Status == "" {
		return "unknown"
	}
	return info.Status
}

// cmdHook is intended to be invoked from a coding agent's hook command (Claude
// Code or Codex — agent selects which). It reads the hook's JSON payload from
// stdin, looks up its own getppid() to identify which agent process called the
// hook, and forwards an enrichment message to the daemon. Both agents share the
// snake_case stdin fields (session_id, transcript_path). Failures are silenced
// so a broken hook can never block the agent.
func cmdHook(c *rpc.Client, event, agent string) {
	body, _ := io.ReadAll(os.Stdin)
	observedAt := time.Now().UTC()
	req := parseHookPayloadAt(body, event, agent, observedAt)
	req.PID = os.Getppid()
	if req.ObservedAt.IsZero() {
		req.ObservedAt = observedAt
	}
	if agent == state.AgentKindCodex {
		req.HookClientHints = hookClientHints()
	}
	_ = c.Send(req)
	var resp rpc.Response
	_ = c.Recv(&resp)
}

type hookPayload struct {
	CWD            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	ToolName       string `json:"tool_name"`
	// ToolInput exists only long enough to become a bounded correlation hash.
	// Raw tool input never leaves parseHookPayload.
	ToolInput            json.RawMessage `json:"tool_input"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	AgentIDAlt           string          `json:"agentId"`
	AgentTypeAlt         string          `json:"agentType"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	Source               string          `json:"source"`
	TurnID               string          `json:"turn_id"`
	ToolUseID            string          `json:"tool_use_id"`
	PermissionMode       string          `json:"permission_mode"`
	// Error is Claude StopFailure's error type. Raw, because another agent's
	// payload may carry a non-string error, which must not fail the decode.
	Error json.RawMessage `json:"error"`
	// ErrorMessage is the Pi extension's errorMessage of a failed run.
	ErrorMessage string `json:"error_message"`
}

// parseHookPayload is the hook privacy boundary. It always returns a sendable
// content-free lifecycle request, even for empty or malformed JSON. Codex
// naming content is admitted only on the two matching lifecycle edges and is
// bounded by Unicode code points before it can reach RPC.
func parseHookPayload(body []byte, event, agent string) rpc.Request {
	return parseHookPayloadAt(body, event, agent, time.Now())
}

// parseHookPayloadAt is parseHookPayload with the hook's observation instant,
// which anchors a usage-limit reset named as a bare clock time.
func parseHookPayloadAt(body []byte, event, agent string, observedAt time.Time) rpc.Request {
	req := rpc.Request{Cmd: "hook", Event: event, Agent: agent}
	if len(body) == 0 {
		return req
	}
	var payload hookPayload
	if json.Unmarshal(body, &payload) != nil {
		return req
	}
	req.SessionID = payload.SessionID
	req.Transcript = payload.TranscriptPath
	if agent == state.AgentKindCodex {
		req.HookCWD = boundedHookPath(payload.CWD)
	}
	req.ToolName = payload.ToolName
	req.ToolInputHash = hashToolInput(payload.ToolInput)
	req.AgentID = firstNonEmpty(payload.AgentID, payload.AgentIDAlt)
	req.AgentType = firstNonEmpty(payload.AgentType, payload.AgentTypeAlt)
	req.HookSource = payload.Source
	req.TurnID = payload.TurnID
	req.ToolUseID = payload.ToolUseID
	req.PermissionMode = payload.PermissionMode
	if agent == state.AgentKindCodex && event == "UserPromptSubmit" {
		req.Prompt = truncatePrompt(payload.Prompt, 1000)
	}
	if agent == state.AgentKindCodex && event == "Stop" {
		req.LastAssistantMessage = truncatePrompt(payload.LastAssistantMessage, 1000)
	}
	if event == "StopFailure" {
		classifyUsageLimit(&req, payload, agent, observedAt)
	}
	if agent == state.AgentKindPi {
		carryPiLifecycle(&req, body, event)
		req.ObservedAt = piEventInstant(body, observedAt)
	}
	return req
}

const (
	maxPiSessionName = 256
	maxPiUsageLabel  = 128
	maxHookPath      = 4096
)

// piHookPayload is the Pi extension's lifecycle metadata. It decodes apart from
// hookPayload so a malformed Pi field cannot cost the shared session identity.
type piHookPayload struct {
	OpenDialogs         *int     `json:"open_dialogs"`
	Busy                bool     `json:"busy"`
	PreviousSessionFile string   `json:"previous_session_file"`
	SessionName         string   `json:"session_name"`
	MessageID           string   `json:"message_id"`
	Provider            string   `json:"provider"`
	Model               string   `json:"model"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	CacheWriteTokens    int64    `json:"cache_write_tokens"`
	TotalTokens         int64    `json:"total_tokens"`
	CostTotal           *float64 `json:"cost_total"`
}

// carryPiLifecycle bounds the Pi extension's metadata onto the request, each
// field only on the event that defines it.
func carryPiLifecycle(req *rpc.Request, body []byte, event string) {
	var payload piHookPayload
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	switch event {
	case "SessionStart":
		req.Busy = payload.Busy
		req.PreviousSessionFile = boundedHookPath(payload.PreviousSessionFile)
		req.SessionName = truncatePrompt(payload.SessionName, maxPiSessionName)
	case "SessionName":
		// Pi's /name mid-session (session_info_changed); empty when cleared.
		req.SessionName = truncatePrompt(payload.SessionName, maxPiSessionName)
	case "PermissionRequest", "PermissionResolved":
		if payload.OpenDialogs != nil {
			count := max(*payload.OpenDialogs, 0)
			req.OpenDialogs = &count
		}
	case "Usage":
		usage := rpc.HookUsage{
			MessageID:        truncatePrompt(payload.MessageID, maxPiUsageLabel),
			Provider:         truncatePrompt(payload.Provider, maxPiUsageLabel),
			Model:            truncatePrompt(payload.Model, maxPiUsageLabel),
			InputTokens:      max(payload.InputTokens, 0),
			OutputTokens:     max(payload.OutputTokens, 0),
			CacheReadTokens:  max(payload.CacheReadTokens, 0),
			CacheWriteTokens: max(payload.CacheWriteTokens, 0),
			TotalTokens:      max(payload.TotalTokens, 0),
		}
		if payload.CostTotal != nil && *payload.CostTotal >= 0 {
			cost := *payload.CostTotal
			usage.CostTotal = &cost
		}
		req.Usage = &usage
	}
}

// The Pi extension stamps every hook with event_at, the instant the Pi event
// fired, because its spawn-and-forget sender lets two hooks reach the daemon in
// either order. The stamp and this process read the same host clock, so a
// sane stamp is at most a few milliseconds behind: the sender kills a ctl that
// has not finished within 2 s. Anything outside these bounds is a broken
// clock or a forged payload, and the ctl's own clock stands in for it.
const (
	// piEventAtMaxFuture tolerates rounding between Date.now() and Go's clock;
	// a stamp up to this far ahead is clamped to now rather than trusted.
	piEventAtMaxFuture = 2 * time.Second
	// piEventAtMaxAge is five times the sender's kill timer.
	piEventAtMaxAge = 10 * time.Second
)

// piEventInstant returns the Pi event's own instant from the payload's
// event_at (Unix milliseconds) when it is sane, else now. It decodes apart
// from the other Pi fields so a malformed stamp costs only itself.
func piEventInstant(body []byte, now time.Time) time.Time {
	var payload struct {
		EventAt *int64 `json:"event_at"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.EventAt == nil {
		return now
	}
	at := time.UnixMilli(*payload.EventAt).UTC()
	if at.After(now.Add(piEventAtMaxFuture)) || at.Before(now.Add(-piEventAtMaxAge)) {
		return now
	}
	if at.After(now) {
		return now
	}
	return at
}

// boundedHookPath admits only a clean absolute path of bounded length.
func boundedHookPath(path string) string {
	if !filepath.IsAbs(path) || len(path) > maxHookPath {
		return ""
	}
	return filepath.Clean(path)
}

// classifyUsageLimit reads a failed turn's error text on this side of the
// privacy boundary and forwards only the verdict and its reset time.
func classifyUsageLimit(req *rpc.Request, payload hookPayload, agent string, at time.Time) {
	var verdict usagelimit.Verdict
	var limited bool
	switch agent {
	case state.AgentKindClaude:
		var errorType string
		if json.Unmarshal(payload.Error, &errorType) != nil {
			return
		}
		verdict, limited = usagelimit.FromClaudeStopFailure(errorType, payload.LastAssistantMessage, at)
	case state.AgentKindPi:
		verdict, limited = usagelimit.FromPiError(payload.ErrorMessage, at)
	}
	if !limited {
		return
	}
	req.UsageLimit = true
	if verdict.ResetsAt != nil {
		resetsAt := verdict.ResetsAt.UTC()
		req.UsageLimitResetsAt = &resetsAt
	}
}
func truncatePrompt(prompt string, limit int) string {
	prompt = strings.TrimSpace(prompt)
	if limit <= 0 {
		return ""
	}
	runes := []rune(prompt)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

const maxHookClientHintLen = 128

// hookClientHints captures only bounded terminal identity metadata that
// Switchboard can independently compare with a discovered TUI. These hints do
// not authorize attribution and are never logged or persisted.
func hookClientHints() []rpc.HookClientHint {
	return hookClientHintsFrom(currentHookTTY(), os.Getenv)
}

func hookClientHintsFrom(tty string, getenv func(string) string) []rpc.HookClientHint {
	var hints []rpc.HookClientHint
	seen := make(map[rpc.HookClientHint]struct{})
	add := func(kind, value string) {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxHookClientHintLen {
			return
		}
		hint := rpc.HookClientHint{Kind: kind, Value: value}
		if _, duplicate := seen[hint]; duplicate {
			return
		}
		seen[hint] = struct{}{}
		hints = append(hints, hint)
	}
	add(rpc.HookClientHintTTY, tty)
	add(rpc.HookClientHintTTY, getenv("SSH_TTY"))
	add(rpc.HookClientHintWeztermPane, getenv("WEZTERM_PANE"))
	add(rpc.HookClientHintTmuxPane, getenv("TMUX_PANE"))
	return hints
}

func currentHookTTY() string {
	for _, fd := range []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()} {
		path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		if err != nil {
			continue
		}
		if strings.HasPrefix(path, "/dev/pts/") || strings.HasPrefix(path, "/dev/tty") {
			return path
		}
	}
	return ""
}

// toolInputHashLen is how much of the sha256 hex digest is forwarded. 16 hex
// chars is 64 bits — far more than enough to tell apart the handful of tool
// calls one writer has in flight at once, and short enough to stay readable in a
// journal line.
const toolInputHashLen = 16

// hashToolInput reduces a hook payload's tool_input to a short, stable
// fingerprint of *which call* it is. The daemon uses it (with agent_id and
// tool_name) to decide whether a PostToolUse is the completion of the very call
// a pending PermissionRequest was raised for.
//
// Two properties matter, and both are why this lives at the ctl edge rather than
// in the daemon:
//
//   - Hash, never forward. tool_input can be large (a whole file body on Write)
//     and can carry sensitive content. Only the digest crosses the socket, and
//     nothing here retains the raw bytes.
//   - Canonicalize before hashing. The same logical call must hash identically
//     when seen via PermissionRequest and again via PostToolUse, and the raw
//     bytes are not guaranteed to be byte-identical across the two emitters —
//     JSON object key order is not significant, and re-serialization may reorder
//     it. So we unmarshal into interface{} and re-marshal: encoding/json sorts
//     map keys on marshal, which normalizes ordering (and whitespace, and string
//     escaping) into one canonical form.
//
// A no-signal input yields "" rather than a hash. Absent, unparseable, null, and
// empty container inputs all return "" — hashing them would mint a digest that
// every other signal-less event also produces, and that false match is exactly
// what this correlator exists to prevent. Consumers must read "" as "no signal",
// never as "a hash that failed to match".
func hashToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	switch v := decoded.(type) {
	case nil: // literal JSON null
		return ""
	case map[string]interface{}:
		if len(v) == 0 {
			return ""
		}
	case []interface{}:
		if len(v) == 0 {
			return ""
		}
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:toolInputHashLen]
}

// firstNonEmpty returns the first non-empty string, for tolerating snake_case vs
// camelCase hook payload keys (e.g. agent_id vs agentId).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// cmdName resolves or edits project abbreviations and pretty display names.
// Subcommands:
//
//	resolve  --cwd <dir> --name <name>   print the prefixed, de-duplicated name
//	abbrev   --cwd <dir>                 print the project's canonical abbrev
//	full     --cwd <dir>                 print the project's pretty display name
//	set      <dir> <abbrev>              persist an abbrev for the dir's git root
//	set-full --cwd <dir> --name <full>   persist a pretty display name for it
func cmdName(args []string) {
	if len(args) == 0 {
		fail("name requires a subcommand: resolve|abbrev|full|set|set-full")
	}
	switch args[0] {
	case "resolve":
		fs := flag.NewFlagSet("name resolve", flag.ExitOnError)
		cwd := fs.String("cwd", "", "project directory (default: current)")
		name := fs.String("name", "", "desired session name to prefix")
		_ = fs.Parse(args[1:])
		fmt.Println(projectname.ResolveForDir(projectname.Load(), dirOrCwd(*cwd), *name))
	case "abbrev":
		fs := flag.NewFlagSet("name abbrev", flag.ExitOnError)
		cwd := fs.String("cwd", "", "project directory (default: current)")
		_ = fs.Parse(args[1:])
		fmt.Println(projectname.CanonicalForDir(projectname.Load(), dirOrCwd(*cwd)))
	case "full":
		fs := flag.NewFlagSet("name full", flag.ExitOnError)
		cwd := fs.String("cwd", "", "project directory (default: current)")
		_ = fs.Parse(args[1:])
		fmt.Println(projectname.FullForDir(projectname.Load(), dirOrCwd(*cwd)))
	case "set":
		rest := args[1:]
		if len(rest) < 2 {
			fail("usage: name set <dir> <abbrev>")
		}
		root := projectname.ProjectRoot(rest[0])
		if err := projectname.SetAbbrev(root, rest[1]); err != nil {
			fail("name set: %v", err)
		}
		fmt.Printf("%s -> %s\n", root, projectname.CanonicalForDir(projectname.Load(), root))
	case "set-full":
		fs := flag.NewFlagSet("name set-full", flag.ExitOnError)
		cwd := fs.String("cwd", "", "project directory (default: current)")
		name := fs.String("name", "", "pretty display name (e.g. \"Switchboard\")")
		_ = fs.Parse(args[1:])
		if strings.TrimSpace(*name) == "" {
			fail("usage: name set-full --cwd <dir> --name <full>")
		}
		root := projectname.ProjectRoot(dirOrCwd(*cwd))
		if err := projectname.SetFull(root, *name); err != nil {
			fail("name set-full: %v", err)
		}
		fmt.Printf("%s -> %s\n", root, projectname.FullForDir(projectname.Load(), root))
	default:
		fail("unknown name subcommand %q (resolve|abbrev|full|set|set-full)", args[0])
	}
}

// dirOrCwd returns dir when non-empty, else the current working directory.
func dirOrCwd(dir string) string {
	if dir != "" {
		return dir
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func mustList(c *rpc.Client) state.Snapshot {
	if err := c.Send(rpc.Request{Cmd: "list-all"}); err != nil {
		fail("send: %v", err)
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		fail("recv: %v", err)
	}
	if resp.Error != "" {
		fail("%s", resp.Error)
	}
	if resp.Snapshot == nil {
		return state.Snapshot{}
	}
	return *resp.Snapshot
}

func usage() {
	fmt.Fprintln(os.Stderr, strings.TrimSpace(`
usage: switchboard-ctl [flags] <cmd> [args]

commands:
  list                    show session list (human-friendly; --json for raw)
  remote-stream           stream bounded local-daemon snapshots as host JSONL;
                            intended for ssh -T <host> switchboard-ctl remote-stream
  focus [selector]        focus exact session: active, idx:<n>, pid:<n>, or
                            host:<hostname>:pid:<n> (required for duplicate PIDs)
  status                  one-line summary
  pick                    emit exact-token<TAB>label<TAB>ws<TAB>cwd for fzf
  cycle next|prev         focus the next/previous session, wrapping
  attention               move right to the most urgent populated colour,
                            wrapping at the end of the bar. If already
                            there, cycle its peers or toggle at most one layer
                            above it. With any red present, orange always jumps
                            to red and green is unreachable. Unknown (grey) and
                            suspended sessions are excluded; cycle remains
                            unrestricted.
  agent-diagnostics       show bounded provider diagnostic counters; --json
                            emits the raw content-free array
  explain --pid <pid>     why one root shows the status it does now: the
                            deciding source, reason code, freshness, and the
                            readings that lost; --json for the record
  name <sub>              project names: resolve --cwd --name, abbrev --cwd,
                            full --cwd, set <dir> <abbrev>, or
                            set-full --cwd --name <full> (pretty display name)
  hook <event>            forward Claude Code hook enrichment (stdin = JSON)
  codex-hook <event>      forward Codex hook enrichment (stdin = JSON)
  pi-hook <event>         forward Pi extension lifecycle (stdin = JSON); see
                            integrations/pi/switchboard.ts
  activity idle|active    report a global user-activity edge for the delegation
                            metrics (idle daemon, e.g. hypridle); session-less
  display mode chips|circles|toggle  choose a presentation (navigation is unchanged)
  display status|watch|serve          inspect mode, stream JSON, or run the broker
  bottombar [sub]         publish the Linux/Waybar bottom strip:
                            publish    attach to one combined Waybar process
                            reconcile-attached
                                       re-derive combined bottom visibility
                            watch      legacy: own a separate bottom process
                            reconcile  legacy: re-derive process visibility
                            stop       legacy: kill the separate bottom process
  diagnose [flags] [desc] explain a wrong chip color, or use --observer for
                            content-free provider binding/graph health. Reads
                            state.json plus the journal; needs no daemon.
  history <sub>           the durable activity log (opt-in): path, tail, stat,
                            purge, calibrate. Reads the on-disk files; needs no
                            daemon.
  timeline [flags]        render the activity log as per-session swimlanes plus
                            attention stats, e.g. timeline --day 2026-06-26.
                            --json emits the structured data; needs no daemon.
  seed-bench [flags]      measure fanout history-seeding cost against a store
                            dir (see scripts/sb-bench-seed); needs no daemon.
  pricing <sub> [flags]   public spot-rate cache: status or refresh; reports
                            source, age, version hash, model count, and fallback.

flags:
  --socket <path>         daemon socket (default: $XDG_RUNTIME_DIR/switchboard.sock)
  --json                  json output for list, agent-diagnostics or explain
`))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "switchboard-ctl: "+format+"\n", args...)
	os.Exit(1)
}

func defaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "switchboard.sock")
	}
	return fmt.Sprintf("/tmp/switchboard-%d.sock", os.Getuid())
}
