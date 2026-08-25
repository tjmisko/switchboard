package codex

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

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

type threadTurnsListResult struct {
	Data            []rpcTurn `json:"data"`
	NextCursor      *string   `json:"nextCursor"`
	BackwardsCursor *string   `json:"backwardsCursor"`
}

type rpcThread struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"sessionId"`
	ParentThreadID string          `json:"parentThreadId"`
	Name           string          `json:"name"`
	AgentNickname  string          `json:"agentNickname"`
	AgentRole      string          `json:"agentRole"`
	ModelProvider  string          `json:"modelProvider"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
	Status         rpcStatus       `json:"status"`
	Source         json.RawMessage `json:"source"`
	Turns          []rpcTurn       `json:"turns"`
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
	Model             string                   `json:"model"`
	ReasoningEffort   string                   `json:"reasoningEffort"`
	SenderThreadID    string                   `json:"senderThreadId"`
	ReceiverThreadIDs []string                 `json:"receiverThreadIds"`
	AgentsStates      map[string]rpcAgentState `json:"agentsStates"`
}

type rpcAgentState struct {
	Status string `json:"status"`
}

type reviewerRoute uint8

const (
	reviewerUnknown reviewerRoute = iota
	reviewerUser
	reviewerAuto
)

type requestOwner uint8

const (
	requestPending requestOwner = iota
	requestHuman
	requestAutomatic
	requestIgnored
)

type rpcRequestID string

type requestEvidence struct {
	reason           agentgraph.AttentionState
	owner            requestOwner
	ownerEvidence    string
	kind             string
	turnID           string
	itemID           string
	episode          uint64
	startedAt        time.Time
	redPublishedAt   time.Time
	redClearedAt     time.Time
	ambiguousAtStart bool
}

type waitOwnershipState struct {
	rawApproval      bool
	rawUserInput     bool
	reviewer         reviewerRoute
	autoReviewSeen   bool
	activeAutoReview map[string]struct{}
	requests         map[rpcRequestID]requestEvidence

	classificationPending bool
	classifiedUnknown     bool
	classificationStarted time.Time
	classificationEpisode uint64
	classificationToken   uint64
	classificationTimer   *time.Timer
}

type nodeState struct {
	node        agentgraph.Node
	baseRuntime agentgraph.RuntimeState
	cohort      string
	guardian    bool
	wait        waitOwnershipState
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
	out := &graphState{
		rootID: s.rootID, rootTurnID: s.rootTurnID,
		nodes:       make(map[string]*nodeState, len(s.nodes)),
		unknownEnum: s.unknownEnum,
	}
	for id, state := range s.nodes {
		copy := *state
		copy.wait.classificationTimer = nil
		copy.wait.classificationPending = false
		copy.wait.classificationStarted = time.Time{}
		copy.wait.classificationEpisode = 0
		copy.wait.activeAutoReview = cloneSet(state.wait.activeAutoReview)
		copy.wait.requests = cloneRequests(state.wait.requests)
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
	state.node.Nickname = strings.TrimSpace(thread.AgentNickname)
	if root {
		state.node.Nickname = strings.TrimSpace(thread.Name)
	}
	state.node.Role = thread.AgentRole
	state.node.Billing.AgentClient = string(agentgraph.ProviderCodex)
	if provider := strings.TrimSpace(thread.ModelProvider); provider != "" {
		state.node.Billing.ExecutionProvider = provider
	}
	if len(thread.Source) > 0 {
		state.guardian = isGuardianSource(thread.Source)
	}
	if startedAt := unixSeconds(thread.CreatedAt); !startedAt.IsZero() {
		state.node.StartedAt = startedAt
	}
	if updatedAt := unixSeconds(thread.UpdatedAt); !updatedAt.IsZero() {
		state.node.UpdatedAt = updatedAt
	}
	if thread.Status.Type != "" || thread.Status.ActiveFlags != nil {
		s.applyStatus(state, thread.Status)
	}
	s.deriveAll()
}

// observeThreadName applies a Codex native-name notification without
// needing a full thread snapshot. The graph carries only Codex's explicit
// user-facing root name; renderers own the short thread-ID fallback used while
// it is unnamed. Child display remains owned by the agent nickname supplied
// with its spawn metadata.
func (s *graphState) observeThreadName(id, name string) bool {
	state := s.nodes[id]
	if state == nil {
		return false
	}
	if id == s.rootID {
		state.node.Nickname = strings.TrimSpace(name)
	}
	return true
}

func (s *graphState) ensureNode(id, parentID string) *nodeState {
	state := s.nodes[id]
	if state == nil {
		state = &nodeState{
			node:        agentgraph.Node{ID: id, ParentID: parentID, Runtime: agentgraph.RuntimeUnknown, Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleUnknown},
			baseRuntime: agentgraph.RuntimeUnknown,
		}
		s.nodes[id] = state
	} else if state.node.ParentID == "" && id != s.rootID && parentID != "" {
		state.node.ParentID = parentID
	}
	return state
}

func (s *graphState) applyStatus(state *nodeState, status rpcStatus) {
	if status.Type != "" {
		state.baseRuntime = mapRuntime(status.Type)
	}
	if status.Type != "" && state.baseRuntime == agentgraph.RuntimeUnknown && status.Type != "unknown" {
		s.unknownEnum = true
	}
	wasWaiting := state.wait.rawApproval || state.wait.rawUserInput
	state.wait.rawApproval = false
	state.wait.rawUserInput = false
	for _, flag := range status.ActiveFlags {
		switch flag {
		case "waitingOnApproval":
			state.wait.rawApproval = true
		case "waitingOnUserInput":
			state.wait.rawUserInput = true
		default:
			s.unknownEnum = true
		}
	}
	isWaiting := state.wait.rawApproval || state.wait.rawUserInput
	if !isWaiting {
		state.clearTransientWait()
	} else if !wasWaiting {
		state.wait.classifiedUnknown = false
	}
	s.deriveAll()
}

func (s *graphState) applyCollaboration(turnID string, item rpcItem) {
	if item.Type != "collabAgentToolCall" {
		return
	}
	parentID := item.SenderThreadID
	for _, id := range item.ReceiverThreadIDs {
		state := s.ensureNode(id, parentID)
		state.node.Billing.AgentClient = string(agentgraph.ProviderCodex)
		if item.Model != "" {
			state.node.Billing.Model = strings.TrimSpace(item.Model)
		}
		if item.ReasoningEffort != "" {
			state.node.Billing.ReasoningEffort = strings.TrimSpace(item.ReasoningEffort)
		}
		if turnID != "" && parentID == s.rootID {
			state.cohort = turnID
		}
	}
	for id, agentState := range item.AgentsStates {
		state := s.ensureNode(id, parentID)
		state.node.Billing.AgentClient = string(agentgraph.ProviderCodex)
		if item.Model != "" {
			state.node.Billing.Model = strings.TrimSpace(item.Model)
		}
		if item.ReasoningEffort != "" {
			state.node.Billing.ReasoningEffort = strings.TrimSpace(item.ReasoningEffort)
		}
		state.node.Lifecycle = mapLifecycle(agentState.Status)
		if state.node.Lifecycle == agentgraph.LifecycleUnknown && agentState.Status != "" && agentState.Status != "unknown" {
			s.unknownEnum = true
		}
		if state.node.Lifecycle.Terminal() {
			state.node.CompletedAt = state.node.UpdatedAt
			state.clearAllWait()
		}
		if turnID != "" && parentID == s.rootID {
			state.cohort = turnID
		}
	}
	s.deriveAll()
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
			s.deleteNode(id)
		}
	}
}

func (s *graphState) deleteThread(id string) {
	children := []string{id}
	for len(children) > 0 {
		parent := children[len(children)-1]
		children = children[:len(children)-1]
		for childID, state := range s.nodes {
			if state.node.ParentID == parent {
				children = append(children, childID)
			}
		}
		s.deleteNode(parent)
	}
}

func (s *graphState) deleteNode(id string) {
	if state := s.nodes[id]; state != nil {
		state.clearAllWait()
	}
	delete(s.nodes, id)
	s.deriveAll()
}

func (s *graphState) deriveAll() {
	for id := range s.nodes {
		s.deriveNode(id)
	}
}

func (s *graphState) deriveNode(id string) {
	state := s.nodes[id]
	if state == nil {
		return
	}
	state.node.Runtime = state.baseRuntime
	state.node.Attention = agentgraph.AttentionNone
	for _, request := range state.wait.requests {
		if request.owner != requestHuman {
			continue
		}
		if request.reason == agentgraph.AttentionApproval {
			state.node.Attention = agentgraph.AttentionApproval
			return
		}
		state.node.Attention = agentgraph.AttentionUserInput
	}
	if state.node.Attention != agentgraph.AttentionNone || (!state.wait.rawApproval && !state.wait.rawUserInput) {
		return
	}
	if s.mechanicalWaitCovered(id) {
		return
	}
	// A mechanical app-server gate with no confirmed owner is deliberately
	// unknown. Runtime "active" is not enough to turn uncertainty green, and the
	// raw waiting flag is not enough to turn it red.
	state.node.Runtime = agentgraph.RuntimeUnknown
}

func (s *graphState) effectiveReviewer(id string) reviewerRoute {
	for current := s.nodes[id]; current != nil; current = s.nodes[current.node.ParentID] {
		if current.wait.reviewer != reviewerUnknown {
			return current.wait.reviewer
		}
		if current.node.ParentID == "" {
			break
		}
	}
	return reviewerUnknown
}

func (s *graphState) hasAutoEvidence(id string) bool {
	state := s.nodes[id]
	if state == nil {
		return false
	}
	if s.effectiveReviewer(id) == reviewerAuto || state.wait.autoReviewSeen || len(state.wait.activeAutoReview) > 0 || state.guardian {
		return true
	}
	for _, candidate := range s.nodes {
		if candidate.node.ParentID == id && candidate.guardian && candidate.baseRuntime == agentgraph.RuntimeActive && !candidate.node.Lifecycle.Terminal() {
			return true
		}
	}
	return false
}

func (s *graphState) autoEvidence(id string) string {
	state := s.nodes[id]
	if state == nil {
		return "automatic"
	}
	if s.effectiveReviewer(id) == reviewerAuto {
		return "reviewer_auto"
	}
	if state.wait.autoReviewSeen || len(state.wait.activeAutoReview) > 0 {
		return "auto_review_event"
	}
	if state.guardian {
		return "guardian_source"
	}
	for _, candidate := range s.nodes {
		if candidate.node.ParentID == id && candidate.guardian && candidate.baseRuntime == agentgraph.RuntimeActive && !candidate.node.Lifecycle.Terminal() {
			return "guardian_source"
		}
	}
	return "automatic"
}

func (s *graphState) mechanicalWaitCovered(id string) bool {
	state := s.nodes[id]
	if state == nil {
		return false
	}
	if s.hasAutoEvidence(id) {
		return true
	}
	approvalCovered := !state.wait.rawApproval
	inputCovered := !state.wait.rawUserInput
	for _, request := range state.wait.requests {
		switch request.reason {
		case agentgraph.AttentionApproval:
			approvalCovered = approvalCovered || request.owner == requestAutomatic
		case agentgraph.AttentionUserInput:
			inputCovered = inputCovered || request.owner == requestAutomatic || request.owner == requestIgnored
		}
	}
	return approvalCovered && inputCovered
}

func (s *graphState) setReviewer(id, value string, now time.Time) bool {
	state := s.nodes[id]
	if state == nil {
		return false
	}
	switch value {
	case "user":
		state.wait.reviewer = reviewerUser
		state.wait.autoReviewSeen = false
		state.wait.activeAutoReview = nil
	case "auto_review", "guardian_subagent":
		state.wait.reviewer = reviewerAuto
	default:
		state.wait.reviewer = reviewerUnknown
		s.unknownEnum = true
	}
	for candidateID := range s.nodes {
		switch s.effectiveReviewer(candidateID) {
		case reviewerUser:
			s.classifyPendingRequests(candidateID, requestHuman, "reviewer_user", now)
		case reviewerAuto:
			s.classifyPendingRequests(candidateID, requestAutomatic, "reviewer_auto", now)
		}
	}
	s.deriveAll()
	return true
}

func (s *graphState) addAutoReview(id, reviewID, targetItemID string, completed bool, now time.Time) bool {
	state := s.nodes[id]
	if state == nil {
		return false
	}
	state.wait.autoReviewSeen = true
	if state.wait.activeAutoReview == nil {
		state.wait.activeAutoReview = make(map[string]struct{})
	}
	if reviewID != "" {
		if completed {
			delete(state.wait.activeAutoReview, reviewID)
		} else {
			state.wait.activeAutoReview[reviewID] = struct{}{}
		}
	}
	matched := false
	for requestID, request := range state.wait.requests {
		if (request.owner != requestPending && !(request.owner == requestHuman && request.ownerEvidence == "timeout_fallback")) ||
			request.reason != agentgraph.AttentionApproval {
			continue
		}
		if targetItemID != "" && request.itemID != targetItemID {
			continue
		}
		request.owner = requestAutomatic
		request.ownerEvidence = "auto_review_event"
		if !request.redPublishedAt.IsZero() && request.redClearedAt.IsZero() {
			request.redClearedAt = now
		}
		state.wait.requests[requestID] = request
		matched = true
	}
	if !matched && targetItemID != "" {
		// The request and unstable review events can arrive in either order. The
		// durable auto-review evidence still classifies the mechanical gate.
		state.wait.autoReviewSeen = true
	}
	s.deriveAll()
	return true
}

func (s *graphState) addRequest(
	id string,
	requestID rpcRequestID,
	reason agentgraph.AttentionState,
	turnID string,
	itemID string,
	kind string,
	episode uint64,
	startedAt time.Time,
	explicitOwner requestOwner,
	explicitEvidence string,
) bool {
	state := s.nodes[id]
	if state == nil || requestID == "" {
		return false
	}
	state.ensureWaitMaps()
	owner, evidence := explicitOwner, explicitEvidence
	switch {
	case owner != requestPending:
	case s.hasAutoEvidence(id):
		owner = requestAutomatic
		evidence = s.autoEvidence(id)
	case s.effectiveReviewer(id) == reviewerUser:
		owner = requestHuman
		evidence = "reviewer_user"
	}
	request := requestEvidence{
		reason: reason, owner: owner, ownerEvidence: evidence, kind: kind,
		turnID: turnID, itemID: itemID, episode: episode, startedAt: startedAt,
		ambiguousAtStart: owner == requestPending,
	}
	if owner == requestHuman {
		request.redPublishedAt = startedAt
	}
	state.wait.requests[requestID] = request
	if owner == requestPending {
		state.wait.classifiedUnknown = false
	}
	s.deriveAll()
	return true
}

func (s *graphState) resolveRequest(id string, requestID rpcRequestID) (requestEvidence, bool) {
	state := s.nodes[id]
	if state == nil || requestID == "" {
		return requestEvidence{}, false
	}
	request, ok := state.wait.requests[requestID]
	if !ok {
		return requestEvidence{}, false
	}
	delete(state.wait.requests, requestID)
	if state.wait.rawApproval || state.wait.rawUserInput {
		state.wait.classifiedUnknown = true
	}
	s.deriveAll()
	return request, true
}

func (s *graphState) classifyPendingRequests(id string, owner requestOwner, evidence string, now time.Time) int {
	state := s.nodes[id]
	if state == nil {
		return 0
	}
	count := 0
	for requestID, request := range state.wait.requests {
		if request.owner != requestPending && !(request.owner == requestHuman && request.ownerEvidence == "timeout_fallback") {
			continue
		}
		request.owner = owner
		request.ownerEvidence = evidence
		if owner == requestHuman && request.redPublishedAt.IsZero() {
			request.redPublishedAt = now
		} else if owner == requestAutomatic && !request.redPublishedAt.IsZero() && request.redClearedAt.IsZero() {
			request.redClearedAt = now
		}
		state.wait.requests[requestID] = request
		count++
	}
	return count
}

func (s *graphState) pendingRequests(id string) map[rpcRequestID]requestEvidence {
	state := s.nodes[id]
	if state == nil {
		return nil
	}
	pending := make(map[rpcRequestID]requestEvidence)
	for requestID, request := range state.wait.requests {
		if request.owner == requestPending || (request.owner == requestHuman && request.ownerEvidence == "timeout_fallback") {
			pending[requestID] = request
		}
	}
	return pending
}

func (s *graphState) request(id string, requestID rpcRequestID) (requestEvidence, bool) {
	state := s.nodes[id]
	if state == nil {
		return requestEvidence{}, false
	}
	request, ok := state.wait.requests[requestID]
	return request, ok
}

func (s *graphState) classificationKind(id string) string {
	state := s.nodes[id]
	if state == nil {
		return "unknown"
	}
	kind := ""
	for _, request := range state.wait.requests {
		if request.owner != requestPending {
			continue
		}
		if kind == "" {
			kind = request.kind
			continue
		}
		if kind != request.kind {
			return "mixed"
		}
	}
	if kind != "" {
		return kind
	}
	switch {
	case state.wait.rawApproval && state.wait.rawUserInput:
		return "raw_mixed_gate"
	case state.wait.rawApproval:
		return "raw_approval_gate"
	case state.wait.rawUserInput:
		return "raw_user_input_gate"
	default:
		return "unknown"
	}
}

func (s *graphState) expireClassification(id string, now time.Time, grace time.Duration) []requestEvidence {
	state := s.nodes[id]
	if state == nil {
		return nil
	}
	var promoted []requestEvidence
	remaining := false
	for requestID, request := range state.wait.requests {
		if request.owner != requestPending {
			continue
		}
		if elapsedSince(request.startedAt, now) < grace {
			remaining = true
			continue
		}
		request.owner = requestHuman
		request.ownerEvidence = "timeout_fallback"
		request.redPublishedAt = now
		state.wait.requests[requestID] = request
		promoted = append(promoted, request)
	}
	state.wait.classificationPending = false
	state.wait.classificationTimer = nil
	state.wait.classificationStarted = time.Time{}
	state.wait.classificationEpisode = 0
	state.wait.classifiedUnknown = len(promoted) == 0 && !remaining
	s.deriveAll()
	return promoted
}

func (s *graphState) needsClassification(id string) bool {
	state := s.nodes[id]
	if state == nil || s.hasAutoEvidence(id) {
		return false
	}
	hasPending := false
	hasHuman := false
	for _, request := range state.wait.requests {
		if request.owner == requestPending {
			hasPending = true
		}
		if request.owner == requestHuman {
			hasHuman = true
		}
	}
	if hasPending {
		return true
	}
	if hasHuman {
		return false
	}
	return (state.wait.rawApproval || state.wait.rawUserInput) && !s.mechanicalWaitCovered(id) && !state.wait.classifiedUnknown
}

func (s *graphState) hasPendingClassification() bool {
	for _, state := range s.nodes {
		if state.wait.classificationPending {
			return true
		}
	}
	return false
}

func (s *graphState) hasHumanAttention() bool {
	for _, state := range s.nodes {
		if state.node.Attention == agentgraph.AttentionApproval || state.node.Attention == agentgraph.AttentionUserInput {
			return true
		}
	}
	return false
}

func (s *graphState) clearThreadWait(id string) bool {
	state := s.nodes[id]
	if state == nil {
		return false
	}
	state.clearAllWait()
	s.deriveAll()
	return true
}

func (state *nodeState) ensureWaitMaps() {
	if state.wait.requests == nil {
		state.wait.requests = make(map[rpcRequestID]requestEvidence)
	}
}

func (state *nodeState) clearTransientWait() {
	state.stopClassification()
	state.wait.rawApproval = false
	state.wait.rawUserInput = false
	state.wait.autoReviewSeen = false
	state.wait.activeAutoReview = nil
	state.wait.requests = nil
	state.wait.classifiedUnknown = false
}

func (state *nodeState) clearAllWait() {
	state.clearTransientWait()
	state.node.Attention = agentgraph.AttentionNone
}

func (state *nodeState) stopClassification() {
	state.wait.classificationToken++
	if state.wait.classificationTimer != nil {
		state.wait.classificationTimer.Stop()
	}
	state.wait.classificationTimer = nil
	state.wait.classificationPending = false
	state.wait.classificationStarted = time.Time{}
	state.wait.classificationEpisode = 0
}

func (s *graphState) stopClassifications() {
	for _, state := range s.nodes {
		state.stopClassification()
	}
}

func (s *graphState) resetWaitOwnership() {
	for _, state := range s.nodes {
		state.stopClassification()
		state.wait = waitOwnershipState{}
	}
	s.deriveAll()
}

func parseRequestID(raw json.RawMessage) (rpcRequestID, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var text string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil || text == "" {
			return "", false
		}
		return rpcRequestID("s:" + text), true
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", false
	}
	return rpcRequestID("n:" + strconv.FormatInt(number, 10)), true
}

func isGuardianSource(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var source struct {
		SubAgent struct {
			Other string `json:"other"`
		} `json:"subAgent"`
	}
	return json.Unmarshal(raw, &source) == nil && source.SubAgent.Other == "guardian"
}

func cloneSet(input map[string]struct{}) map[string]struct{} {
	if input == nil {
		return nil
	}
	out := make(map[string]struct{}, len(input))
	for key := range input {
		out[key] = struct{}{}
	}
	return out
}

func cloneRequests(input map[rpcRequestID]requestEvidence) map[rpcRequestID]requestEvidence {
	if input == nil {
		return nil
	}
	out := make(map[rpcRequestID]requestEvidence, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
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
			s.deleteNode(old.id)
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
