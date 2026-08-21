package codex

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

type initializeResult struct {
	UserAgent string `json:"userAgent"`
}

type threadReadResult struct {
	Thread rpcThread `json:"thread"`
}

type threadListResult struct {
	Data       []rpcThread `json:"data"`
	NextCursor *string     `json:"nextCursor"`
}

type rpcThread struct {
	ID             string    `json:"id"`
	ParentThreadID string    `json:"parentThreadId"`
	Name           string    `json:"name"`
	Preview        string    `json:"preview"`
	AgentNickname  string    `json:"agentNickname"`
	AgentRole      string    `json:"agentRole"`
	CreatedAt      int64     `json:"createdAt"`
	UpdatedAt      int64     `json:"updatedAt"`
	Status         rpcStatus `json:"status"`
	Turns          []rpcTurn `json:"turns"`
}

type rpcStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags"`
}

type rpcTurn struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Items       []rpcItem `json:"items"`
	StartedAt   int64     `json:"startedAt"`
	CompletedAt int64     `json:"completedAt"`
}

type rpcItem struct {
	ID                string                   `json:"id"`
	Type              string                   `json:"type"`
	Tool              string                   `json:"tool"`
	Status            string                   `json:"status"`
	SenderThreadID    string                   `json:"senderThreadId"`
	ReceiverThreadIDs []string                 `json:"receiverThreadIds"`
	AgentsStates      map[string]rpcAgentState `json:"agentsStates"`
}

type rpcAgentState struct {
	Status string `json:"status"`
}

type nodeState struct {
	node          agentgraph.Node
	cohort        string
	threadName    string
	threadPreview string
}

type graphState struct {
	rootID      string
	rootTurnID  string
	nodes       map[string]*nodeState
	unknownEnum bool
}

func newGraphState(root rpcThread, descendants []rpcThread, terminalLimit int) *graphState {
	state := &graphState{rootID: root.ID, nodes: make(map[string]*nodeState, len(descendants)+1)}
	state.upsertThread(root, true)
	for _, thread := range descendants {
		state.upsertThread(thread, false)
	}
	for _, turn := range root.Turns {
		state.rootTurnID = turn.ID
		for _, item := range turn.Items {
			state.applyCollaboration(turn.ID, item)
		}
	}
	for _, thread := range descendants {
		for _, turn := range thread.Turns {
			for _, item := range turn.Items {
				state.applyCollaboration(turn.ID, item)
			}
		}
	}
	state.boundTerminals(terminalLimit)
	return state
}

func (s *graphState) clone() *graphState {
	out := &graphState{rootID: s.rootID, rootTurnID: s.rootTurnID, nodes: make(map[string]*nodeState, len(s.nodes)), unknownEnum: s.unknownEnum}
	for id, state := range s.nodes {
		copy := *state
		out.nodes[id] = &copy
	}
	return out
}

func (s *graphState) upsertThread(thread rpcThread, root bool) {
	if thread.ID == "" {
		return
	}
	state := s.ensureNode(thread.ID, thread.ParentThreadID)
	if root {
		state.node.ParentID = ""
	} else if thread.ParentThreadID != "" {
		state.node.ParentID = thread.ParentThreadID
	}
	state.threadName = strings.TrimSpace(thread.Name)
	state.threadPreview = thread.Preview
	state.node.Nickname = strings.TrimSpace(thread.AgentNickname)
	if root {
		state.node.Nickname = threadDisplayName(state.threadName, state.threadPreview)
	}
	state.node.Role = thread.AgentRole
	if startedAt := unixSeconds(thread.CreatedAt); !startedAt.IsZero() {
		state.node.StartedAt = startedAt
	}
	if updatedAt := unixSeconds(thread.UpdatedAt); !updatedAt.IsZero() {
		state.node.UpdatedAt = updatedAt
	}
	if thread.Status.Type != "" || thread.Status.ActiveFlags != nil {
		s.applyStatus(state, thread.Status)
	}
}

// setThreadName applies Codex's thread/name/updated notification without
// needing a full thread snapshot. Root names fall back to the stable preview
// slug when a user clears an explicit name; child display remains owned by the
// agent nickname supplied with its spawn metadata.
func (s *graphState) setThreadName(id, name string) bool {
	state := s.nodes[id]
	if state == nil {
		return false
	}
	state.threadName = strings.TrimSpace(name)
	if id == s.rootID {
		state.node.Nickname = threadDisplayName(state.threadName, state.threadPreview)
	}
	return true
}

// threadDisplayName prefers Codex's explicit user-facing thread name. Unnamed
// threads otherwise expose a UUID in the configurable terminal-title item, so
// Switchboard derives a short, deterministic slug from the first-message
// preview instead. This is display metadata only: it never renames the Codex
// thread or participates in status reduction.
func threadDisplayName(name, preview string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return previewSlug(preview)
}

const (
	previewSlugWords = 5
	previewSlugRunes = 40
)

var previewStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "can": {}, "could": {},
	"d": {}, "for": {}, "from": {}, "help": {}, "i": {},
	"in": {}, "is": {}, "it": {}, "like": {}, "me": {}, "my": {},
	"of": {}, "on": {}, "please": {}, "that": {}, "the": {}, "this": {},
	"to": {}, "us": {}, "want": {}, "we": {}, "with": {}, "would": {},
	"you": {}, "your": {},
}

// previewSlug extracts a compact task label from an unnamed thread's first
// user message. It keeps the first useful sentence, drops conversational glue,
// and has hard word/rune caps so prompt text cannot balloon a bar chip. Unicode
// letters and digits are retained; punctuation becomes a word boundary.
func previewSlug(preview string) string {
	words := make([]string, 0, previewSlugWords)
	var token strings.Builder

	flush := func() {
		if token.Len() == 0 || len(words) == previewSlugWords {
			token.Reset()
			return
		}
		word := strings.ToLower(token.String())
		token.Reset()
		if _, skip := previewStopWords[word]; skip {
			return
		}
		words = append(words, word)
	}

	for _, r := range strings.TrimSpace(preview) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			token.WriteRune(r)
			continue
		}
		flush()
		if len(words) == previewSlugWords || ((r == '.' || r == '!' || r == '?' || r == '\n') && len(words) >= 2) {
			break
		}
	}
	flush()
	if len(words) == 0 {
		return ""
	}

	var slug strings.Builder
	usedRunes := 0
	for _, word := range words {
		separator := 0
		if usedRunes > 0 {
			separator = 1
		}
		remaining := previewSlugRunes - usedRunes - separator
		if remaining <= 0 {
			break
		}
		wordRunes := []rune(word)
		if len(wordRunes) > remaining {
			wordRunes = wordRunes[:remaining]
		}
		if usedRunes > 0 {
			slug.WriteByte('-')
			usedRunes++
		}
		slug.WriteString(string(wordRunes))
		usedRunes += len(wordRunes)
		if len(wordRunes) < len([]rune(word)) {
			break
		}
	}
	return strings.Trim(slug.String(), "-")
}

func (s *graphState) ensureNode(id, parentID string) *nodeState {
	state := s.nodes[id]
	if state == nil {
		state = &nodeState{node: agentgraph.Node{ID: id, ParentID: parentID, Runtime: agentgraph.RuntimeUnknown, Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleUnknown}}
		s.nodes[id] = state
	} else if state.node.ParentID == "" && id != s.rootID && parentID != "" {
		state.node.ParentID = parentID
	}
	return state
}

func (s *graphState) applyStatus(state *nodeState, status rpcStatus) {
	state.node.Runtime = mapRuntime(status.Type)
	if status.Type != "" && state.node.Runtime == agentgraph.RuntimeUnknown && status.Type != "unknown" {
		s.unknownEnum = true
	}
	state.node.Attention = agentgraph.AttentionNone
	for _, flag := range status.ActiveFlags {
		switch flag {
		case "waitingOnApproval":
			state.node.Attention = agentgraph.AttentionApproval
		case "waitingOnUserInput":
			if state.node.Attention != agentgraph.AttentionApproval {
				state.node.Attention = agentgraph.AttentionUserInput
			}
		default:
			s.unknownEnum = true
		}
	}
}

func (s *graphState) applyCollaboration(turnID string, item rpcItem) {
	if item.Type != "collabAgentToolCall" {
		return
	}
	parentID := item.SenderThreadID
	for _, id := range item.ReceiverThreadIDs {
		state := s.ensureNode(id, parentID)
		if turnID != "" && parentID == s.rootID {
			state.cohort = turnID
		}
	}
	for id, agentState := range item.AgentsStates {
		state := s.ensureNode(id, parentID)
		state.node.Lifecycle = mapLifecycle(agentState.Status)
		if state.node.Lifecycle == agentgraph.LifecycleUnknown && agentState.Status != "" && agentState.Status != "unknown" {
			s.unknownEnum = true
		}
		if state.node.Lifecycle.Terminal() {
			state.node.CompletedAt = state.node.UpdatedAt
		}
		if turnID != "" && parentID == s.rootID {
			state.cohort = turnID
		}
	}
}

func (s *graphState) beginRootTurn(turnID string) {
	if turnID == "" || turnID == s.rootTurnID {
		return
	}
	s.rootTurnID = turnID
	required := make(map[string]bool)
	for id, state := range s.nodes {
		if id == s.rootID || state.node.Lifecycle.Terminal() {
			continue
		}
		for parentID := state.node.ParentID; parentID != ""; {
			required[parentID] = true
			parent := s.nodes[parentID]
			if parent == nil {
				break
			}
			parentID = parent.node.ParentID
		}
	}
	for id, state := range s.nodes {
		if id != s.rootID && state.node.Lifecycle.Terminal() && state.cohort != turnID && !required[id] {
			delete(s.nodes, id)
		}
	}
}

func (s *graphState) deleteThread(id string) {
	children := []string{id}
	for len(children) > 0 {
		parent := children[len(children)-1]
		children = children[:len(children)-1]
		delete(s.nodes, parent)
		for childID, state := range s.nodes {
			if state.node.ParentID == parent {
				children = append(children, childID)
			}
		}
	}
}

func (s *graphState) boundTerminals(limit int) {
	if limit < 0 {
		limit = 0
	}
	type candidate struct {
		id string
		at time.Time
	}
	var terminal []candidate
	for id, state := range s.nodes {
		if id != s.rootID && state.node.Lifecycle.Terminal() && (s.rootTurnID == "" || state.cohort != s.rootTurnID) {
			terminal = append(terminal, candidate{id: id, at: state.node.UpdatedAt})
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].at.Equal(terminal[j].at) {
			return terminal[i].id > terminal[j].id
		}
		return terminal[i].at.After(terminal[j].at)
	})
	if len(terminal) <= limit {
		return
	}
	required := make(map[string]bool)
	for id, state := range s.nodes {
		if id != s.rootID && !state.node.Lifecycle.Terminal() {
			required[id] = true
		}
	}
	for _, retained := range terminal[:limit] {
		required[retained.id] = true
	}
	for id := range required {
		state := s.nodes[id]
		for state != nil && state.node.ParentID != "" {
			required[state.node.ParentID] = true
			state = s.nodes[state.node.ParentID]
		}
	}
	for _, old := range terminal[limit:] {
		if !required[old.id] {
			delete(s.nodes, old.id)
		}
	}
}

func (s *graphState) observation(now time.Time, freshness time.Duration) (agentgraph.Observation, error) {
	observation := agentgraph.Observation{
		Provider: agentgraph.ProviderCodex, RootID: s.rootID, Source: agentgraph.SourceCodexAppServer,
		ObservedAt: now, FreshUntil: now.Add(freshness), Complete: true,
		Nodes: make([]agentgraph.Node, 0, len(s.nodes)),
	}
	if s.unknownEnum {
		observation.Diagnostic = "Codex app-server returned an unknown enum value"
	}
	for _, state := range s.nodes {
		observation.Nodes = append(observation.Nodes, state.node)
	}
	return agentgraph.Normalize(observation)
}

func mapRuntime(value string) agentgraph.RuntimeState {
	switch value {
	case "notLoaded":
		return agentgraph.RuntimeNotLoaded
	case "idle":
		return agentgraph.RuntimeIdle
	case "active":
		return agentgraph.RuntimeActive
	case "systemError":
		return agentgraph.RuntimeSystemError
	default:
		return agentgraph.RuntimeUnknown
	}
}

func mapLifecycle(value string) agentgraph.LifecycleState {
	switch value {
	case "pendingInit":
		return agentgraph.LifecyclePending
	case "running":
		return agentgraph.LifecycleRunning
	case "completed":
		return agentgraph.LifecycleCompleted
	case "interrupted":
		return agentgraph.LifecycleInterrupted
	case "errored":
		return agentgraph.LifecycleErrored
	case "shutdown":
		return agentgraph.LifecycleShutdown
	case "notFound":
		return agentgraph.LifecycleNotFound
	default:
		return agentgraph.LifecycleUnknown
	}
}

func unixSeconds(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func decodeParams[T any](raw json.RawMessage) (T, bool) {
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, false
	}
	return value, true
}
