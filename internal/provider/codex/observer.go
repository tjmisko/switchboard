// Package codex observes Codex app-server state through the read-only
// `codex app-server proxy` surface and projects it onto the neutral agent graph.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

const (
	DefaultFreshness          = 15 * time.Second
	DefaultResnapshot         = 10 * time.Second
	DefaultRequestTimeout     = 5 * time.Second
	DefaultReconnectMinimum   = 100 * time.Millisecond
	DefaultReconnectMaximum   = 5 * time.Second
	DefaultTerminalLimit      = 32
	DefaultWaitClassification = 500 * time.Millisecond

	DiagnosticUnknownProtocolEnum    = "unknown_protocol_enum"
	DiagnosticSnapshotThreadRead     = "snapshot_thread_read_error"
	DiagnosticSnapshotRootMismatch   = "snapshot_root_mismatch"
	DiagnosticSnapshotThreadList     = "snapshot_thread_list_error"
	DiagnosticSnapshotGraphInvalid   = "snapshot_graph_invalid"
	DiagnosticSnapshotUnknownFailure = "snapshot_unknown_error"
)

// descendantSourceKinds is explicit because thread/list otherwise defaults to
// interactive cli/vscode threads and silently excludes Codex descendants.
// These are all subagent source kinds in the accepted 0.149 protocol.
var descendantSourceKinds = []string{
	"subAgent",
	"subAgentReview",
	"subAgentCompact",
	"subAgentThreadSpawn",
	"subAgentOther",
}

// Config controls observer I/O and bounded retention. Zero durations receive
// production defaults. Connector and Environment are injectable so tests never
// touch an installed Codex binary, socket, or live process environment.
type Config struct {
	Connector           Connector
	Environment         EnvironmentReader
	Freshness           time.Duration
	ResnapshotInterval  time.Duration
	RequestTimeout      time.Duration
	ReconnectMinimum    time.Duration
	ReconnectMaximum    time.Duration
	UpdateBuffer        int
	RecentTerminalLimit int
	WaitClassification  time.Duration
	Now                 func() time.Time
	Jitter              func(time.Duration) time.Duration
	// Diagnostic receives finite categories only; raw protocol errors and
	// payloads never cross this callback.
	Diagnostic     func(string)
	WaitDiagnostic func(WaitClassificationDiagnostic)
}

// WaitClassificationDiagnostic contains finite-label, content-free evidence
// about one ownership decision. It never contains thread/request IDs, prompts,
// commands, reasons, or auto-review rationale.
type WaitClassificationDiagnostic struct {
	Source             string
	Duration           time.Duration
	SuppressedFalseRed bool
}

type rootRecord struct {
	threadID    string
	binding     BindingSource
	graph       *graphState
	observation agentgraph.Observation
	generation  uint64
	expiry      *time.Timer
}

// Observer is a supervised provider.Observer. NewObserver starts its proxy
// supervisor immediately; callers should register exact hook bindings before
// their first Observe when process-environment binding is unavailable.
type Observer struct {
	config   Config
	bindings *BindingRegistry
	queue    *provider.InvalidationQueue

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once

	mu              sync.Mutex
	roots           map[provider.RootKey]*rootRecord
	generation      uint64
	connected       bool
	syncing         bool
	queued          []rpcNotification
	pendingStatuses map[string]rpcStatus
	pendingWaits    map[string][]rpcNotification
	closed          bool
	lastDiagnostics map[string]time.Time

	refresh chan struct{}
}

var _ provider.Observer = (*Observer)(nil)

// NewObserver constructs and starts a Codex observer.
func NewObserver(config Config) *Observer {
	config = withDefaults(config)
	ctx, cancel := context.WithCancel(context.Background())
	observer := &Observer{
		config: config, bindings: newBindingRegistry(config.Environment),
		queue: provider.NewInvalidationQueue(config.UpdateBuffer), ctx: ctx, cancel: cancel,
		roots: make(map[provider.RootKey]*rootRecord), pendingStatuses: make(map[string]rpcStatus),
		pendingWaits:    make(map[string][]rpcNotification),
		lastDiagnostics: make(map[string]time.Time),
		refresh:         make(chan struct{}, 1),
	}
	observer.wg.Add(1)
	go observer.run()
	return observer
}

func withDefaults(config Config) Config {
	if config.Connector == nil {
		config.Connector = CommandConnector{}
	}
	if config.Freshness <= 0 {
		config.Freshness = DefaultFreshness
	}
	if config.ResnapshotInterval <= 0 {
		config.ResnapshotInterval = DefaultResnapshot
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = DefaultRequestTimeout
	}
	if config.ReconnectMinimum <= 0 {
		config.ReconnectMinimum = DefaultReconnectMinimum
	}
	if config.ReconnectMaximum < config.ReconnectMinimum {
		config.ReconnectMaximum = DefaultReconnectMaximum
		if config.ReconnectMaximum < config.ReconnectMinimum {
			config.ReconnectMaximum = config.ReconnectMinimum
		}
	}
	if config.UpdateBuffer <= 0 {
		config.UpdateBuffer = 64
	}
	if config.RecentTerminalLimit == 0 {
		config.RecentTerminalLimit = DefaultTerminalLimit
	}
	if config.WaitClassification <= 0 {
		config.WaitClassification = DefaultWaitClassification
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Jitter == nil {
		config.Jitter = func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(max) + 1))
		}
	}
	return config
}

// RegisterHookBinding records a daemon-verified exact thread ID. It supersedes
// the immutable process-start CODEX_THREAD_ID so /clear can rotate the thread
// under a stable TUI process. Registration triggers a resnapshot and is safe to
// call before or after the first Observe.
func (o *Observer) RegisterHookBinding(key provider.RootKey, threadID string) error {
	if err := o.bindings.RegisterHook(key, threadID); err != nil {
		return err
	}
	o.signalRefresh()
	return nil
}

// Observe resolves exact identity outside the cache mutex and returns a deep
// copy of the last complete snapshot. Expiration is represented by the
// observation's unchanged FreshUntil boundary; consumers use neutral freshness
// reduction rather than a guessed fallback graph.
func (o *Observer) Observe(ctx context.Context, ref provider.RootRef, _ time.Time) (agentgraph.Observation, error) {
	if ref.Provider != "" && ref.Provider != agentgraph.ProviderCodex {
		return agentgraph.Observation{}, fmt.Errorf("codex observer cannot observe provider %q", ref.Provider)
	}
	if ref.PID <= 0 || ref.StartedAt.IsZero() {
		return agentgraph.Observation{Provider: agentgraph.ProviderCodex, Complete: false, Diagnostic: "process start identity unavailable"}, nil
	}
	binding, diagnostic := o.bindings.resolve(ctx, ref)
	if binding.ThreadID == "" {
		return agentgraph.Observation{Provider: agentgraph.ProviderCodex, Complete: false, Diagnostic: diagnostic}, nil
	}

	key := ref.Key()
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return agentgraph.Observation{}, errors.New("codex observer is closed")
	}
	record := o.roots[key]
	changed := false
	if record == nil {
		record = &rootRecord{threadID: binding.ThreadID, binding: binding.Source}
		o.roots[key] = record
		changed = true
	} else if record.threadID != binding.ThreadID {
		if record.expiry != nil {
			record.expiry.Stop()
		}
		record = &rootRecord{threadID: binding.ThreadID, binding: binding.Source}
		o.roots[key] = record
		changed = true
	}
	observation := record.observation.Clone()
	if observation.RootID == "" {
		observation = agentgraph.Observation{
			Provider: agentgraph.ProviderCodex, RootID: binding.ThreadID,
			Source: agentgraph.SourceCodexAppServer, Complete: false,
			Diagnostic: "Codex app-server snapshot pending",
		}
	}
	o.mu.Unlock()
	if changed {
		o.signalRefresh()
	}
	return observation, nil
}

func (o *Observer) Updates() <-chan provider.RootKey { return o.queue.Updates() }

// Forget drops all binding, graph, and timer state for one process lifetime.
// It is idempotent and does not affect another process that reused the PID.
func (o *Observer) Forget(key provider.RootKey) {
	o.bindings.Forget(key)
	o.mu.Lock()
	if record := o.roots[key]; record != nil {
		if record.expiry != nil {
			record.expiry.Stop()
		}
		if record.graph != nil {
			record.graph.stopClassifications()
		}
	}
	delete(o.roots, key)
	o.mu.Unlock()
}

// Close is idempotent. It cancels the proxy child, releases request waiters,
// stops retry/freshness timers, and waits for the supervisor goroutine.
func (o *Observer) Close() error {
	o.once.Do(func() {
		o.cancel()
		o.wg.Wait()
		o.mu.Lock()
		o.closed = true
		for _, record := range o.roots {
			if record.expiry != nil {
				record.expiry.Stop()
			}
			if record.graph != nil {
				record.graph.stopClassifications()
			}
		}
		o.mu.Unlock()
	})
	return nil
}

func (o *Observer) run() {
	defer o.wg.Done()
	backoff := o.config.ReconnectMinimum
	for {
		if o.ctx.Err() != nil {
			return
		}
		connection, err := o.config.Connector.Connect(o.ctx)
		if err != nil {
			if !o.waitBackoff(backoff) {
				return
			}
			backoff = min(backoff*2, o.config.ReconnectMaximum)
			continue
		}
		o.mu.Lock()
		o.generation++
		generation := o.generation
		o.connected = true
		o.syncing = true
		o.queued = nil
		o.pendingStatuses = make(map[string]rpcStatus)
		o.pendingWaits = make(map[string][]rpcNotification)
		for _, record := range o.roots {
			if record.graph != nil {
				record.graph.resetWaitOwnership()
			}
		}
		o.mu.Unlock()

		client := newRPCClient(connection, generation, o.handleNotification)
		if err := o.initialize(client); err != nil {
			_ = client.Close()
			o.disconnect(generation)
			if !o.waitBackoff(backoff) {
				return
			}
			backoff = min(backoff*2, o.config.ReconnectMaximum)
			continue
		}
		o.resnapshotAll(client, generation)
		o.finishSync(generation)

		ticker := time.NewTicker(o.config.ResnapshotInterval)
		stableAfter := max(time.Second, o.config.ReconnectMinimum*4)
		stableTimer := time.NewTimer(stableAfter)
		stable := stableTimer.C
		connected := true
		for connected {
			select {
			case <-o.ctx.Done():
				ticker.Stop()
				stableTimer.Stop()
				_ = client.Close()
				o.disconnect(generation)
				return
			case <-client.Done():
				ticker.Stop()
				stableTimer.Stop()
				_ = client.Close()
				o.disconnect(generation)
				if !o.waitBackoff(backoff) {
					return
				}
				backoff = min(backoff*2, o.config.ReconnectMaximum)
				connected = false
			case <-stable:
				backoff = o.config.ReconnectMinimum
				stable = nil
			case <-o.refresh:
				o.beginSync(generation)
				o.resnapshotAll(client, generation)
				o.finishSync(generation)
			case <-ticker.C:
				o.beginSync(generation)
				o.resnapshotAll(client, generation)
				o.finishSync(generation)
			}
		}
	}
}

func (o *Observer) initialize(client *rpcClient) error {
	ctx, cancel := context.WithTimeout(o.ctx, o.config.RequestTimeout)
	defer cancel()
	var result initializeResult
	if err := client.request(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "switchboard", "title": "Switchboard", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &result); err != nil {
		return err
	}
	if err := checkAppServerVersion(result.UserAgent); err != nil {
		return err
	}
	return client.notify("initialized", map[string]any{})
}

func checkAppServerVersion(userAgent string) error {
	if userAgent == "" {
		return errors.New("codex app-server did not report a version")
	}
	return checkProxyVersion(strings.ReplaceAll(userAgent, "/", " "))
}

func (o *Observer) resnapshotAll(client *rpcClient, generation uint64) {
	type target struct {
		key provider.RootKey
		id  string
	}
	o.mu.Lock()
	targets := make([]target, 0, len(o.roots))
	for key, record := range o.roots {
		targets = append(targets, target{key: key, id: record.threadID})
	}
	o.mu.Unlock()
	for _, target := range targets {
		state, err := o.snapshot(client, target.id)
		if err != nil {
			o.emitDiagnostic(snapshotDiagnosticCategory(err))
			continue
		}
		o.installSnapshot(generation, target.key, target.id, state)
	}
}

type snapshotDiagnosticError struct {
	category string
	err      error
}

func newSnapshotDiagnosticError(category string, err error) error {
	return &snapshotDiagnosticError{category: category, err: err}
}

func (e *snapshotDiagnosticError) Error() string { return e.err.Error() }
func (e *snapshotDiagnosticError) Unwrap() error { return e.err }

func snapshotDiagnosticCategory(err error) string {
	var diagnostic *snapshotDiagnosticError
	if errors.As(err, &diagnostic) {
		return diagnostic.category
	}
	return DiagnosticSnapshotUnknownFailure
}

func (o *Observer) snapshot(client *rpcClient, rootID string) (*graphState, error) {
	ctx, cancel := context.WithTimeout(o.ctx, o.config.RequestTimeout)
	defer cancel()
	var rootResult threadReadResult
	if err := client.request(ctx, "thread/read", map[string]any{"threadId": rootID, "includeTurns": true}, &rootResult); err != nil {
		return nil, newSnapshotDiagnosticError(DiagnosticSnapshotThreadRead, err)
	}
	if rootResult.Thread.ID != rootID {
		return nil, newSnapshotDiagnosticError(DiagnosticSnapshotRootMismatch, errors.New("codex app-server returned a different root thread"))
	}
	var descendants []rpcThread
	var cursor string
	for {
		params := map[string]any{
			"ancestorThreadId": rootID, "archived": false, "limit": 100,
			"sortKey": "created_at", "sortDirection": "asc", "useStateDbOnly": true,
			"sourceKinds": descendantSourceKinds,
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result threadListResult
		if err := client.request(ctx, "thread/list", params, &result); err != nil {
			return nil, newSnapshotDiagnosticError(DiagnosticSnapshotThreadList, err)
		}
		descendants = append(descendants, result.Data...)
		if result.NextCursor == nil || *result.NextCursor == "" {
			break
		}
		cursor = *result.NextCursor
	}
	state := newGraphState(rootResult.Thread, descendants, o.config.RecentTerminalLimit)
	if _, err := state.observation(o.config.Now(), o.config.Freshness); err != nil {
		return nil, newSnapshotDiagnosticError(DiagnosticSnapshotGraphInvalid, err)
	}
	return state, nil
}

func (o *Observer) installSnapshot(generation uint64, key provider.RootKey, threadID string, state *graphState) {
	now := o.config.Now()
	observation, err := state.observation(now, o.config.Freshness)
	if err != nil {
		return
	}
	o.mu.Lock()
	record := o.roots[key]
	if o.generation != generation || record == nil || record.threadID != threadID {
		o.mu.Unlock()
		return
	}
	if record.graph != nil {
		record.graph.stopClassifications()
	}
	record.graph = state
	record.generation = generation
	diagnostics := o.reconcileClassificationsLocked(key, record, "snapshot")
	publish := !state.hasPendingClassification() || state.hasHumanAttention()
	if publish {
		record.observation = observation
		o.scheduleExpiryLocked(key, record)
	}
	o.mu.Unlock()
	if publish {
		o.queue.Signal(key)
	}
	o.emitClassificationDiagnostics(diagnostics)
	o.emitUnknownDiagnostic(state)
}

func (o *Observer) beginSync(generation uint64) {
	o.mu.Lock()
	if o.generation == generation && o.connected {
		o.syncing = true
		o.queued = nil
	}
	o.mu.Unlock()
}

func (o *Observer) finishSync(generation uint64) {
	o.mu.Lock()
	if o.generation != generation || !o.connected {
		o.mu.Unlock()
		return
	}
	queued := o.queued
	o.queued = nil
	o.syncing = false
	o.mu.Unlock()
	for _, notification := range queued {
		o.handleNotification(notification)
	}
}

func (o *Observer) disconnect(generation uint64) {
	o.mu.Lock()
	if o.generation != generation {
		o.mu.Unlock()
		return
	}
	o.connected = false
	o.syncing = false
	o.queued = nil
	o.pendingStatuses = make(map[string]rpcStatus)
	o.pendingWaits = make(map[string][]rpcNotification)
	keys := make([]provider.RootKey, 0, len(o.roots))
	for key, record := range o.roots {
		if record.graph != nil {
			record.graph.stopClassifications()
		}
		keys = append(keys, key)
	}
	o.mu.Unlock()
	for _, key := range keys {
		o.queue.Signal(key)
	}
}

func (o *Observer) handleNotification(notification rpcNotification) {
	o.mu.Lock()
	if notification.Generation != o.generation || !o.connected {
		o.mu.Unlock()
		return
	}
	if o.syncing {
		notification.ID = append(json.RawMessage(nil), notification.ID...)
		notification.Params = append(json.RawMessage(nil), notification.Params...)
		o.queued = append(o.queued, notification)
		o.mu.Unlock()
		return
	}
	keys, unknown, diagnostics := o.applyNotificationLocked(notification)
	o.mu.Unlock()
	for _, key := range keys {
		o.queue.Signal(key)
	}
	if unknown {
		o.emitUnknownDiagnostic(&graphState{unknownEnum: true})
	}
	o.emitClassificationDiagnostics(diagnostics)
}

type notificationParams struct {
	ThreadID       string            `json:"threadId"`
	ConversationID string            `json:"conversationId"`
	ThreadName     *string           `json:"threadName"`
	TurnID         string            `json:"turnId"`
	ItemID         string            `json:"itemId"`
	CallID         string            `json:"callId"`
	ReviewID       string            `json:"reviewId"`
	TargetItemID   string            `json:"targetItemId"`
	RequestID      json.RawMessage   `json:"requestId"`
	IsBlocking     bool              `json:"isBlocking"`
	AutoResolution *uint64           `json:"autoResolutionMs"`
	Thread         rpcThread         `json:"thread"`
	Status         rpcStatus         `json:"status"`
	Turn           rpcTurn           `json:"turn"`
	Item           rpcItem           `json:"item"`
	ThreadSettings rpcThreadSettings `json:"threadSettings"`
}

type rpcThreadSettings struct {
	ApprovalsReviewer string `json:"approvalsReviewer"`
}

func (o *Observer) applyNotificationLocked(notification rpcNotification) ([]provider.RootKey, bool, []WaitClassificationDiagnostic) {
	params, ok := decodeParams[notificationParams](notification.Params)
	if !ok {
		return nil, false, nil
	}
	var changed []provider.RootKey
	var diagnostics []WaitClassificationDiagnostic
	unknown := false
	statusMatched := false
	eventMatched := false
	for key, record := range o.roots {
		if record.graph == nil || record.generation != notification.Generation {
			continue
		}
		state := record.graph
		touches := false
		forcePublish := false
		classificationSource := "protocol_event"
		switch notification.Method {
		case "thread/started", "thread/updated":
			thread := params.Thread
			if thread.ID == "" {
				continue
			}
			if thread.ID == state.rootID || state.nodes[thread.ParentThreadID] != nil || state.nodes[thread.ID] != nil {
				state.upsertThread(thread, thread.ID == state.rootID)
				if pending, exists := o.pendingStatuses[thread.ID]; exists {
					state.applyStatus(state.nodes[thread.ID], pending)
					delete(o.pendingStatuses, thread.ID)
				}
				if isGuardianSource(thread.Source) {
					classificationSource = "guardian_source"
				}
				touches = true
			}
		case "thread/status/changed":
			if node := state.nodes[params.ThreadID]; node != nil {
				state.applyStatus(node, params.Status)
				node.node.UpdatedAt = o.config.Now()
				touches = true
				statusMatched = true
			}
		case "thread/name/updated":
			name := ""
			if params.ThreadName != nil {
				name = *params.ThreadName
			}
			if state.setThreadName(params.ThreadID, name) {
				state.nodes[params.ThreadID].node.UpdatedAt = o.config.Now()
				touches = true
			}
		case "turn/started":
			if node := state.nodes[params.ThreadID]; node != nil {
				if params.ThreadID == state.rootID {
					state.beginRootTurn(params.Turn.ID)
				}
				node.baseRuntime = agentgraph.RuntimeActive
				touches = true
			}
		case "turn/completed":
			if state.clearThreadWait(params.ThreadID) {
				classificationSource = "turn_completed"
				touches = true
			}
		case "item/started", "item/completed":
			if state.nodes[params.ThreadID] != nil || state.nodes[params.Item.SenderThreadID] != nil {
				state.applyCollaboration(params.TurnID, params.Item)
				for _, childID := range params.Item.ReceiverThreadIDs {
					if pending, exists := o.pendingStatuses[childID]; exists {
						if child := state.nodes[childID]; child != nil {
							state.applyStatus(child, pending)
							delete(o.pendingStatuses, childID)
						}
					}
				}
				touches = true
			}
		case "thread/archived", "thread/deleted":
			if params.ThreadID == state.rootID && state.clearThreadWait(params.ThreadID) {
				classificationSource = "thread_completed"
				touches = true
			} else if state.nodes[params.ThreadID] != nil {
				state.deleteThread(params.ThreadID)
				classificationSource = "thread_completed"
				touches = true
			}
		case "thread/settings/updated":
			if state.setReviewer(params.ThreadID, params.ThreadSettings.ApprovalsReviewer) {
				if state.effectiveReviewer(params.ThreadID) == reviewerAuto {
					classificationSource = "reviewer_auto"
				} else if state.effectiveReviewer(params.ThreadID) == reviewerUser {
					classificationSource = "reviewer_user"
					forcePublish = state.hasHumanAttention()
				} else {
					classificationSource = "reviewer_unknown"
				}
				touches = true
			}
		case "item/autoApprovalReview/started", "item/autoApprovalReview/completed":
			if state.addAutoReview(params.ThreadID, params.ReviewID, params.TargetItemID, notification.Method == "item/autoApprovalReview/completed") {
				classificationSource = "auto_review_event"
				touches = true
			}
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
			requestID, valid := parseRequestID(notification.ID)
			if valid && state.addRequest(params.ThreadID, requestID, agentgraph.AttentionApproval, params.TurnID, params.ItemID, false, false) {
				classificationSource = "approval_request"
				forcePublish = state.hasHumanAttention()
				touches = true
			}
		case "applyPatchApproval", "execCommandApproval":
			requestID, valid := parseRequestID(notification.ID)
			if valid && state.addRequest(params.ConversationID, requestID, agentgraph.AttentionApproval, "", params.CallID, false, false) {
				classificationSource = "approval_request"
				forcePublish = state.hasHumanAttention()
				touches = true
			}
		case "item/tool/requestUserInput":
			requestID, valid := parseRequestID(notification.ID)
			autoResolving := params.AutoResolution != nil
			if valid && state.addRequest(params.ThreadID, requestID, agentgraph.AttentionUserInput, params.TurnID, params.ItemID, params.IsBlocking && !autoResolving, !params.IsBlocking || autoResolving) {
				classificationSource = "user_input_request"
				forcePublish = state.hasHumanAttention()
				touches = true
			}
		case "mcpServer/elicitation/request":
			requestID, valid := parseRequestID(notification.ID)
			if valid && state.addRequest(params.ThreadID, requestID, agentgraph.AttentionUserInput, params.TurnID, params.ItemID, true, false) {
				classificationSource = "user_input_request"
				forcePublish = true
				touches = true
			}
		case "serverRequest/resolved":
			requestID, valid := parseRequestID(params.RequestID)
			if valid && state.resolveRequest(params.ThreadID, requestID) {
				classificationSource = "request_resolved"
				touches = true
			}
		}
		if !touches {
			continue
		}
		eventMatched = true
		diagnostics = append(diagnostics, o.reconcileClassificationsLocked(key, record, classificationSource)...)
		if state.hasPendingClassification() && !forcePublish {
			unknown = unknown || state.unknownEnum
			continue
		}
		now := o.config.Now()
		observation, err := state.observation(now, o.config.Freshness)
		if err != nil {
			continue
		}
		record.observation = observation
		o.scheduleExpiryLocked(key, record)
		changed = append(changed, key)
		unknown = unknown || state.unknownEnum
	}
	if notification.Method == "thread/status/changed" && params.ThreadID != "" && !statusMatched {
		if len(o.pendingStatuses) >= 256 {
			for id := range o.pendingStatuses {
				delete(o.pendingStatuses, id)
				break
			}
		}
		o.pendingStatuses[params.ThreadID] = params.Status
	}
	if (notification.Method == "turn/completed" || notification.Method == "thread/archived" || notification.Method == "thread/deleted") && params.ThreadID != "" {
		delete(o.pendingWaits, params.ThreadID)
		if notification.Method != "turn/completed" {
			delete(o.pendingStatuses, params.ThreadID)
		}
	}
	if !eventMatched {
		if threadID := pendingWaitThread(notification.Method, params); threadID != "" {
			o.retainPendingWaitLocked(threadID, notification)
		}
	}
	if (notification.Method == "thread/started" || notification.Method == "thread/updated") && params.Thread.ID != "" {
		pending := o.pendingWaits[params.Thread.ID]
		delete(o.pendingWaits, params.Thread.ID)
		for _, retained := range pending {
			retainedChanged, retainedUnknown, retainedDiagnostics := o.applyNotificationLocked(retained)
			for _, key := range retainedChanged {
				changed = appendUniqueRootKey(changed, key)
			}
			unknown = unknown || retainedUnknown
			diagnostics = append(diagnostics, retainedDiagnostics...)
		}
	}
	return changed, unknown, diagnostics
}

func pendingWaitThread(method string, params notificationParams) string {
	switch method {
	case "thread/settings/updated", "item/autoApprovalReview/started", "item/autoApprovalReview/completed",
		"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval",
		"item/tool/requestUserInput", "mcpServer/elicitation/request", "serverRequest/resolved":
		return params.ThreadID
	case "applyPatchApproval", "execCommandApproval":
		return params.ConversationID
	default:
		return ""
	}
}

func (o *Observer) retainPendingWaitLocked(threadID string, notification rpcNotification) {
	if o.pendingWaits == nil {
		o.pendingWaits = make(map[string][]rpcNotification)
	}
	count := 0
	for _, pending := range o.pendingWaits {
		count += len(pending)
	}
	if count >= 256 {
		for id, pending := range o.pendingWaits {
			if len(pending) <= 1 {
				delete(o.pendingWaits, id)
			} else {
				o.pendingWaits[id] = pending[1:]
			}
			break
		}
	}
	notification.ID = append(json.RawMessage(nil), notification.ID...)
	notification.Params = append(json.RawMessage(nil), notification.Params...)
	o.pendingWaits[threadID] = append(o.pendingWaits[threadID], notification)
}

func appendUniqueRootKey(keys []provider.RootKey, candidate provider.RootKey) []provider.RootKey {
	for _, key := range keys {
		if key == candidate {
			return keys
		}
	}
	return append(keys, candidate)
}

func (o *Observer) reconcileClassificationsLocked(key provider.RootKey, record *rootRecord, source string) []WaitClassificationDiagnostic {
	if record == nil || record.graph == nil {
		return nil
	}
	state := record.graph
	state.deriveAll()
	var diagnostics []WaitClassificationDiagnostic
	for id, node := range state.nodes {
		needs := state.needsClassification(id)
		switch {
		case needs && !node.wait.classificationPending:
			o.startClassificationLocked(key, record, id, node)
		case !needs && node.wait.classificationPending:
			started := node.wait.classificationStarted
			node.stopClassification()
			diagnostics = append(diagnostics, classificationDiagnostic(
				source, elapsedSince(started, o.config.Now()), suppressesFalseRed(source),
			))
		}
	}
	state.deriveAll()
	return diagnostics
}

func (o *Observer) startClassificationLocked(key provider.RootKey, record *rootRecord, threadID string, node *nodeState) {
	delay := o.config.WaitClassification
	if delay <= 0 {
		delay = DefaultWaitClassification
	}
	node.wait.classificationToken++
	token := node.wait.classificationToken
	node.wait.classificationPending = true
	node.wait.classificationStarted = o.config.Now()
	graph := record.graph
	generation := record.generation
	node.wait.classificationTimer = time.AfterFunc(delay, func() {
		o.mu.Lock()
		current := o.roots[key]
		if o.closed || current != record || current.graph != graph || current.generation != generation {
			o.mu.Unlock()
			return
		}
		currentNode := graph.nodes[threadID]
		if currentNode == nil || !currentNode.wait.classificationPending || currentNode.wait.classificationToken != token {
			o.mu.Unlock()
			return
		}
		started := currentNode.wait.classificationStarted
		becameHuman := graph.expireClassification(threadID)
		publish := !graph.hasPendingClassification() || graph.hasHumanAttention()
		if publish {
			now := o.config.Now()
			observation, err := graph.observation(now, o.config.Freshness)
			if err == nil {
				current.observation = observation
				o.scheduleExpiryLocked(key, current)
			} else {
				publish = false
			}
		}
		o.mu.Unlock()
		if publish {
			o.queue.Signal(key)
		}
		source := "unknown_timeout"
		if becameHuman {
			source = "request_timeout"
		}
		o.emitClassificationDiagnostics([]WaitClassificationDiagnostic{
			classificationDiagnostic(source, elapsedSince(started, o.config.Now()), false),
		})
	})
}

func elapsedSince(started, now time.Time) time.Duration {
	if started.IsZero() || now.Before(started) {
		return 0
	}
	return now.Sub(started)
}

func suppressesFalseRed(source string) bool {
	switch source {
	case "reviewer_auto", "auto_review_event", "guardian_source":
		return true
	default:
		return false
	}
}

func classificationDiagnostic(source string, duration time.Duration, suppressed bool) WaitClassificationDiagnostic {
	return WaitClassificationDiagnostic{Source: source, Duration: duration, SuppressedFalseRed: suppressed}
}

func (o *Observer) emitClassificationDiagnostics(diagnostics []WaitClassificationDiagnostic) {
	if o.config.WaitDiagnostic == nil {
		return
	}
	for _, diagnostic := range diagnostics {
		o.config.WaitDiagnostic(diagnostic)
	}
}

func (o *Observer) scheduleExpiryLocked(key provider.RootKey, record *rootRecord) {
	if record.expiry != nil {
		record.expiry.Stop()
	}
	deadline := record.observation.FreshUntil
	delay := deadline.Sub(o.config.Now())
	if delay < 0 {
		delay = 0
	}
	record.expiry = time.AfterFunc(delay, func() {
		o.mu.Lock()
		current := o.roots[key]
		valid := !o.closed && current == record && current.observation.FreshUntil.Equal(deadline)
		o.mu.Unlock()
		if valid {
			o.queue.Signal(key)
		}
	})
}

func (o *Observer) emitUnknownDiagnostic(state *graphState) {
	if !state.unknownEnum {
		return
	}
	o.emitDiagnostic(DiagnosticUnknownProtocolEnum)
}

func (o *Observer) emitDiagnostic(category string) {
	if o.config.Diagnostic == nil {
		return
	}
	o.mu.Lock()
	now := o.config.Now()
	if o.lastDiagnostics == nil {
		o.lastDiagnostics = make(map[string]time.Time)
	}
	if last := o.lastDiagnostics[category]; !last.IsZero() && now.Sub(last) < time.Minute {
		o.mu.Unlock()
		return
	}
	o.lastDiagnostics[category] = now
	o.mu.Unlock()
	o.config.Diagnostic(category)
}

func (o *Observer) signalRefresh() {
	select {
	case o.refresh <- struct{}{}:
	default:
	}
}

func (o *Observer) waitBackoff(backoff time.Duration) bool {
	delay := backoff + o.config.Jitter(backoff/2)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-o.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
