// Package codex observes Codex app-server state through a disposable standalone
// `codex app-server --stdio` process and projects it onto the neutral agent graph.
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
	DefaultActiveResnapshot   = 1 * time.Second
	DefaultIdleResnapshot     = 10 * time.Second
	DefaultRequestTimeout     = 5 * time.Second
	DefaultReconnectMinimum   = 100 * time.Millisecond
	DefaultReconnectMaximum   = 5 * time.Second
	DefaultTerminalLimit      = 32
	DefaultWaitClassification = 30 * time.Second
	legacyWaitClassification  = 500 * time.Millisecond

	DiagnosticUnknownProtocolEnum     = "unknown_protocol_enum"
	DiagnosticSnapshotThreadRead      = "snapshot_thread_read_error"
	DiagnosticSnapshotTurnsList       = "snapshot_turns_list_error"
	DiagnosticSnapshotRootMismatch    = "snapshot_root_mismatch"
	DiagnosticSnapshotThreadList      = "snapshot_thread_list_error"
	DiagnosticSnapshotGraphInvalid    = "snapshot_graph_invalid"
	DiagnosticSnapshotUnknownFailure  = "snapshot_unknown_error"
	DiagnosticObserverConnect         = "observer_connect_error"
	DiagnosticObserverConnectAttempt  = "observer_connect_attempt"
	DiagnosticObserverSupervisorStart = "observer_supervisor_started"
	DiagnosticObserverConnected       = "observer_connected"
	DiagnosticInitializeRequest       = "observer_initialize_request_error"
	DiagnosticInitializeVersion       = "observer_initialize_version_error"
	DiagnosticInitializedNotify       = "observer_initialized_notify_error"
	DiagnosticObserverInitialized     = "observer_initialized"
	DiagnosticConnectionLost          = "observer_connection_lost"
	DiagnosticSnapshotNoTargets       = "snapshot_no_targets"
	DiagnosticSnapshotTargetsPresent  = "snapshot_targets_present"
	DiagnosticSnapshotInstalled       = "snapshot_installed"
	DiagnosticSnapshotRootNotLoaded   = "snapshot_root_not_loaded"
	DiagnosticSnapshotChildrenPresent = "snapshot_children_present"
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
// production defaults. Connector is injectable so tests never touch an
// installed Codex binary or live app-server process.
type Config struct {
	Connector           Connector
	Freshness           time.Duration
	ResnapshotInterval  time.Duration
	ActivePollInterval  time.Duration
	IdlePollInterval    time.Duration
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
// about one wait episode. It never contains thread/request IDs, prompts,
// commands, reasons, tool input, or auto-review rationale. Episode is an
// observer-local correlation number and has no relationship to a Codex ID.
type WaitClassificationDiagnostic struct {
	Event                       string
	Episode                     uint64
	RequestKind                 string
	Ownership                   string
	Evidence                    string
	Source                      string
	Duration                    time.Duration
	RedDuration                 time.Duration
	RedPublished                bool
	HumanEvidence               bool
	ClearedWithoutHumanEvidence bool
	SuppressedFalseRed          bool
	LegacyWouldPublishRed       bool
}

type rootRecord struct {
	threadID    string
	binding     BindingSource
	graph       *graphState
	observation agentgraph.Observation
	generation  uint64
	expiry      *time.Timer
}

// Observer is a supervised provider.Observer. NewObserver starts its standalone
// app-server supervisor immediately; callers register exact hook bindings before
// their first Observe. It performs no environment or loaded-thread attribution.
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
	waitEpisode     uint64
	closed          bool
	lastDiagnostics map[string]time.Time

	refresh chan struct{}
	cadence chan struct{}
}

var _ provider.Observer = (*Observer)(nil)

// NewObserver constructs and starts a Codex observer.
func NewObserver(config Config) *Observer {
	config = withDefaults(config)
	ctx, cancel := context.WithCancel(context.Background())
	observer := &Observer{
		config: config, bindings: newBindingRegistry(),
		queue: provider.NewInvalidationQueue(config.UpdateBuffer), ctx: ctx, cancel: cancel,
		roots: make(map[provider.RootKey]*rootRecord), pendingStatuses: make(map[string]rpcStatus),
		pendingWaits:    make(map[string][]rpcNotification),
		lastDiagnostics: make(map[string]time.Time),
		refresh:         make(chan struct{}, 1),
		cadence:         make(chan struct{}, 1),
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
	if config.ResnapshotInterval > 0 {
		config.ActivePollInterval = config.ResnapshotInterval
		config.IdlePollInterval = config.ResnapshotInterval
	} else {
		if config.ActivePollInterval <= 0 {
			config.ActivePollInterval = DefaultActiveResnapshot
		}
		if config.IdlePollInterval <= 0 {
			config.IdlePollInterval = DefaultIdleResnapshot
		}
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

// RegisterHookBinding records a daemon-verified exact thread ID. A later hook
// can rotate the binding after /clear under the same process lifetime.
func (o *Observer) RegisterHookBinding(key provider.RootKey, threadID string) error {
	_, err := o.ReconcileHookBinding(key, threadID)
	return err
}

// ReconcileHookBinding exposes rotation/stale classification to the daemon
// while preserving RegisterHookBinding for older observer implementations.
func (o *Observer) ReconcileHookBinding(key provider.RootKey, threadID string) (BindingUpdate, error) {
	update, err := o.bindings.RegisterHook(key, threadID)
	if err != nil {
		return BindingUpdate{}, err
	}
	o.signalRefresh()
	return update, nil
}

// Observe resolves exact identity outside the cache mutex and returns a deep
// copy of the last complete snapshot. Expiration is represented by the
// observation's unchanged FreshUntil boundary; consumers use neutral freshness
// reduction rather than a guessed fallback graph.
func (o *Observer) Observe(_ context.Context, ref provider.RootRef, _ time.Time) (agentgraph.Observation, error) {
	if ref.Provider != "" && ref.Provider != agentgraph.ProviderCodex {
		return agentgraph.Observation{}, fmt.Errorf("codex observer cannot observe provider %q", ref.Provider)
	}
	if ref.PID <= 0 || ref.StartedAt.IsZero() {
		return agentgraph.Observation{Provider: agentgraph.ProviderCodex, Complete: false, Diagnostic: "process start identity unavailable"}, nil
	}
	binding, diagnostic := o.bindings.resolve(ref)
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

// Close is idempotent. It cancels the standalone app-server child, releases request waiters,
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
	o.emitDiagnostic(DiagnosticObserverSupervisorStart)
	backoff := o.config.ReconnectMinimum
	for {
		if o.ctx.Err() != nil {
			return
		}
		o.emitDiagnostic(DiagnosticObserverConnectAttempt)
		connection, err := o.config.Connector.Connect(o.ctx)
		if err != nil {
			o.emitDiagnostic(DiagnosticObserverConnect)
			if !o.waitBackoff(backoff) {
				return
			}
			backoff = min(backoff*2, o.config.ReconnectMaximum)
			continue
		}
		o.emitDiagnostic(DiagnosticObserverConnected)
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
		o.emitDiagnostic(DiagnosticObserverInitialized)
		o.resnapshotAll(client, generation)
		o.finishSync(generation)

		ticker := time.NewTicker(o.pollInterval())
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
				if o.ctx.Err() == nil {
					o.emitDiagnostic(DiagnosticConnectionLost)
				}
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
				ticker.Reset(o.pollInterval())
			case <-o.cadence:
				ticker.Reset(o.pollInterval())
			case <-ticker.C:
				o.beginSync(generation)
				o.resnapshotAll(client, generation)
				o.finishSync(generation)
				ticker.Reset(o.pollInterval())
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
		o.emitDiagnostic(DiagnosticInitializeRequest)
		return err
	}
	if err := checkAppServerVersion(result.UserAgent); err != nil {
		o.emitDiagnostic(DiagnosticInitializeVersion)
		return err
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		o.emitDiagnostic(DiagnosticInitializedNotify)
		return err
	}
	return nil
}

func checkAppServerVersion(userAgent string) error {
	if userAgent == "" {
		return errors.New("codex app-server did not report a version")
	}
	return checkAppServerCLIVersion(strings.ReplaceAll(userAgent, "/", " "))
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
	if len(targets) == 0 {
		o.emitDiagnostic(DiagnosticSnapshotNoTargets)
	} else {
		o.emitDiagnostic(DiagnosticSnapshotTargetsPresent)
	}
	for _, target := range targets {
		if target.id == "" {
			continue
		}
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
	o.emitSnapshotTurnEvidence("thread_read", rootResult.Thread.Turns)
	var turnResult threadTurnsListResult
	if err := client.request(ctx, "thread/turns/list", map[string]any{
		"threadId": rootID, "limit": 100, "sortDirection": "desc", "itemsView": "full",
	}, &turnResult); err != nil {
		// This request is supplemental characterization evidence. The already
		// successful thread/read plus descendant list remain a valid structural
		// snapshot when an installed CLI lacks or rejects the method.
		o.emitDiagnostic(DiagnosticSnapshotTurnsList)
	} else {
		o.emitSnapshotTurnEvidence("turns_list", turnResult.Data)
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
		o.emitDiagnostic(DiagnosticSnapshotInstalled)
		if len(observation.Nodes) > 0 && observation.Nodes[0].Runtime == agentgraph.RuntimeNotLoaded {
			o.emitDiagnostic(DiagnosticSnapshotRootNotLoaded)
		}
		if len(observation.Nodes) > 1 {
			o.emitDiagnostic(DiagnosticSnapshotChildrenPresent)
		}
		o.emitSnapshotChildEvidence(observation)
	}
	o.emitClassificationDiagnostics(diagnostics)
	o.emitUnknownDiagnostic(state)
}

// emitSnapshotTurnEvidence reports only finite structural/lifecycle classes.
// It deliberately excludes root/thread/item IDs, prompts, messages, tool input,
// and every raw provider value. The two source labels are internal constants.
func (o *Observer) emitSnapshotTurnEvidence(source string, turns []rpcTurn) {
	for _, category := range snapshotTurnEvidenceCategories(source, turns) {
		o.emitDiagnostic(category)
	}
}

func snapshotTurnEvidenceCategories(source string, turns []rpcTurn) []string {
	prefix := "snapshot_" + source
	if len(turns) == 0 {
		return []string{prefix + "_turns_absent"}
	}
	categories := []string{prefix + "_turns_present"}
	collabItems, collabStates, receivers := false, false, false
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.Type != "collabAgentToolCall" {
				continue
			}
			collabItems = true
			if len(item.ReceiverThreadIDs) > 0 {
				receivers = true
			}
			categories = appendUniqueDiagnostic(categories,
				prefix+"_collab_tool_"+collabToolDiagnosticSuffix(item.Tool),
				prefix+"_collab_call_"+collabCallDiagnosticSuffix(item.Status),
			)
			for _, state := range item.AgentsStates {
				collabStates = true
				lifecycle := mapLifecycle(state.Status)
				categories = appendUniqueDiagnostic(categories, prefix+"_collab_state_"+string(lifecycle))
			}
		}
	}
	if !collabItems {
		return append(categories, prefix+"_collab_items_absent")
	}
	categories = append(categories, prefix+"_collab_items_present")
	if receivers {
		categories = append(categories, prefix+"_collab_receivers_present")
	} else {
		categories = append(categories, prefix+"_collab_receivers_absent")
	}
	if collabStates {
		categories = append(categories, prefix+"_collab_states_present")
	} else {
		categories = append(categories, prefix+"_collab_states_absent")
	}
	return categories
}

func collabToolDiagnosticSuffix(value string) string {
	switch value {
	case "spawnAgent":
		return "spawn_agent"
	case "sendInput":
		return "send_input"
	case "resumeAgent":
		return "resume_agent"
	case "wait":
		return "wait"
	case "closeAgent":
		return "close_agent"
	default:
		return "unknown"
	}
}

func collabCallDiagnosticSuffix(value string) string {
	switch value {
	case "inProgress":
		return "in_progress"
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	default:
		return "unknown"
	}
}

func appendUniqueDiagnostic(categories []string, candidates ...string) []string {
	for _, candidate := range candidates {
		found := false
		for _, category := range categories {
			if category == candidate {
				found = true
				break
			}
		}
		if !found {
			categories = append(categories, candidate)
		}
	}
	return categories
}

func (o *Observer) emitSnapshotChildEvidence(observation agentgraph.Observation) {
	for _, node := range observation.Nodes {
		if node.ID == observation.RootID {
			continue
		}
		o.emitDiagnostic("snapshot_child_runtime_" + string(node.Runtime))
		o.emitDiagnostic("snapshot_child_lifecycle_" + string(node.Lifecycle))
	}
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
	evidenceCategory := notificationEvidenceCategory(notification)
	o.mu.Lock()
	if notification.Generation != o.generation || !o.connected {
		o.mu.Unlock()
		if evidenceCategory != "" {
			o.emitDiagnostic(evidenceCategory + "_stale")
		}
		return
	}
	if o.syncing {
		notification.ID = append(json.RawMessage(nil), notification.ID...)
		notification.Params = append(json.RawMessage(nil), notification.Params...)
		o.queued = append(o.queued, notification)
		o.mu.Unlock()
		if evidenceCategory != "" {
			o.emitDiagnostic(evidenceCategory + "_queued")
		}
		return
	}
	keys, unknown, diagnostics := o.applyNotificationLocked(notification)
	o.mu.Unlock()
	if evidenceCategory != "" {
		if len(keys) > 0 {
			o.emitDiagnostic(evidenceCategory + "_matched")
		} else {
			o.emitDiagnostic(evidenceCategory + "_unmatched")
		}
	}
	for _, key := range keys {
		o.queue.Signal(key)
	}
	select {
	case o.cadence <- struct{}{}:
	default:
	}
	if unknown {
		o.emitUnknownDiagnostic(&graphState{unknownEnum: true})
	}
	o.emitClassificationDiagnostics(diagnostics)
}

// notificationEvidenceCategory recognizes only lifecycle-relevant method and
// item classes. It never incorporates an arbitrary provider method or payload
// value into logs.
func (o *Observer) pollInterval() time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, record := range o.roots {
		for _, node := range record.observation.Nodes {
			if node.ID == record.threadID && node.Runtime == agentgraph.RuntimeActive {
				return o.config.ActivePollInterval
			}
		}
	}
	return o.config.IdlePollInterval
}

func notificationEvidenceCategory(notification rpcNotification) string {
	switch notification.Method {
	case "thread/started":
		return "notification_thread_started"
	case "thread/updated":
		return "notification_thread_updated"
	case "thread/status/changed":
		return "notification_thread_status"
	case "turn/started":
		return "notification_turn_started"
	case "turn/completed":
		return "notification_turn_completed"
	case "thread/archived":
		return "notification_thread_archived"
	case "thread/deleted":
		return "notification_thread_deleted"
	case "item/started", "item/completed":
		params, ok := decodeParams[notificationParams](notification.Params)
		if !ok || params.Item.Type != "collabAgentToolCall" {
			return ""
		}
		if notification.Method == "item/started" {
			return "notification_collab_item_started"
		}
		return "notification_collab_item_completed"
	default:
		return ""
	}
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
	eventAt := o.config.Now()
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
				pending := state.pendingRequests(thread.ParentThreadID)
				state.upsertThread(thread, thread.ID == state.rootID)
				if pending, exists := o.pendingStatuses[thread.ID]; exists {
					state.applyStatus(state.nodes[thread.ID], pending)
					delete(o.pendingStatuses, thread.ID)
				}
				if isGuardianSource(thread.Source) {
					classificationSource = "guardian_source"
					state.classifyPendingRequests(thread.ParentThreadID, requestAutomatic, classificationSource, eventAt)
					diagnostics = append(diagnostics, classifiedRequestDiagnostics(state, thread.ParentThreadID, pending, classificationSource, eventAt)...)
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
			if state.observeThreadName(params.ThreadID, name) {
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
			pending := state.pendingRequests(params.ThreadID)
			if state.setReviewer(params.ThreadID, params.ThreadSettings.ApprovalsReviewer, eventAt) {
				if state.effectiveReviewer(params.ThreadID) == reviewerAuto {
					classificationSource = "reviewer_auto"
				} else if state.effectiveReviewer(params.ThreadID) == reviewerUser {
					classificationSource = "reviewer_user"
					forcePublish = state.hasHumanAttention()
				} else {
					classificationSource = "reviewer_unknown"
				}
				diagnostics = append(diagnostics, classifiedRequestDiagnostics(state, params.ThreadID, pending, classificationSource, eventAt)...)
				touches = true
			}
		case "item/autoApprovalReview/started", "item/autoApprovalReview/completed":
			pending := state.pendingRequests(params.ThreadID)
			if state.addAutoReview(params.ThreadID, params.ReviewID, params.TargetItemID, notification.Method == "item/autoApprovalReview/completed", eventAt) {
				classificationSource = "auto_review_event"
				diagnostics = append(diagnostics, classifiedRequestDiagnostics(state, params.ThreadID, pending, classificationSource, eventAt)...)
				touches = true
			}
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
			requestID, valid := parseRequestID(notification.ID)
			episode := o.nextWaitEpisodeLocked()
			if valid && state.addRequest(params.ThreadID, requestID, agentgraph.AttentionApproval, params.TurnID, params.ItemID,
				approvalRequestKind(notification.Method), episode, eventAt, requestPending, "unknown") {
				classificationSource = "approval_request"
				if request, exists := state.request(params.ThreadID, requestID); exists {
					diagnostics = append(diagnostics, requestStartDiagnostics(request, eventAt)...)
				}
				forcePublish = state.hasHumanAttention()
				touches = true
			}
		case "applyPatchApproval", "execCommandApproval":
			requestID, valid := parseRequestID(notification.ID)
			episode := o.nextWaitEpisodeLocked()
			if valid && state.addRequest(params.ConversationID, requestID, agentgraph.AttentionApproval, "", params.CallID,
				approvalRequestKind(notification.Method), episode, eventAt, requestPending, "unknown") {
				classificationSource = "approval_request"
				if request, exists := state.request(params.ConversationID, requestID); exists {
					diagnostics = append(diagnostics, requestStartDiagnostics(request, eventAt)...)
				}
				forcePublish = state.hasHumanAttention()
				touches = true
			}
		case "item/tool/requestUserInput":
			requestID, valid := parseRequestID(notification.ID)
			autoResolving := params.AutoResolution != nil
			owner, evidence := requestIgnored, "nonblocking_input"
			if params.IsBlocking && !autoResolving {
				owner, evidence = requestHuman, "blocking_user_input"
			} else if autoResolving {
				evidence = "auto_resolving_input"
			}
			episode := o.nextWaitEpisodeLocked()
			if valid && state.addRequest(params.ThreadID, requestID, agentgraph.AttentionUserInput, params.TurnID, params.ItemID,
				"user_input", episode, eventAt, owner, evidence) {
				classificationSource = "user_input_request"
				if request, exists := state.request(params.ThreadID, requestID); exists {
					diagnostics = append(diagnostics, requestStartDiagnostics(request, eventAt)...)
				}
				forcePublish = state.hasHumanAttention()
				touches = true
			}
		case "mcpServer/elicitation/request":
			requestID, valid := parseRequestID(notification.ID)
			episode := o.nextWaitEpisodeLocked()
			if valid && state.addRequest(params.ThreadID, requestID, agentgraph.AttentionUserInput, params.TurnID, params.ItemID,
				"mcp_elicitation", episode, eventAt, requestHuman, "mcp_elicitation") {
				classificationSource = "user_input_request"
				if request, exists := state.request(params.ThreadID, requestID); exists {
					diagnostics = append(diagnostics, requestStartDiagnostics(request, eventAt)...)
				}
				forcePublish = true
				touches = true
			}
		case "serverRequest/resolved":
			requestID, valid := parseRequestID(params.RequestID)
			if request, resolved := state.resolveRequest(params.ThreadID, requestID); valid && resolved {
				classificationSource = "request_resolved"
				diagnostics = append(diagnostics, requestDiagnostic("resolved", request, classificationSource, eventAt))
				touches = true
			}
		}
		if !touches {
			continue
		}
		eventMatched = true
		diagnostics = append(diagnostics, o.reconcileClassificationsLocked(key, record, classificationSource)...)
		// Never let a different ambiguous request hold a previously published red
		// edge after its owning request resolved or became automatic. Publishing
		// gray here is preferable to retaining stale human attention; the pending
		// request will regain its prior/nonattention projection when classified.
		mustClearStaleAttention := observationHasHumanAttention(record.observation) && !state.hasHumanAttention()
		if state.hasPendingClassification() && !forcePublish && !mustClearStaleAttention {
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

func observationHasHumanAttention(observation agentgraph.Observation) bool {
	for _, node := range observation.Nodes {
		if node.Attention == agentgraph.AttentionApproval || node.Attention == agentgraph.AttentionUserInput {
			return true
		}
	}
	return false
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
			diagnostics = append(diagnostics, o.startClassificationLocked(key, record, id, node)...)
		case !needs && node.wait.classificationPending:
			started := node.wait.classificationStarted
			episode := node.wait.classificationEpisode
			kind := state.classificationKind(id)
			node.stopClassification()
			// Request-backed waits already emit their own correlated evidence or
			// resolution event. classificationEpisode is reserved for raw status
			// gates that have no request ID, avoiding duplicate wait episodes.
			if episode != 0 {
				diagnostics = append(diagnostics, classificationDiagnostic(
					"evidence", episode, kind, source, elapsedSince(started, o.config.Now()), suppressesFalseRed(source),
				))
			}
		}
	}
	state.deriveAll()
	return diagnostics
}

func (o *Observer) startClassificationLocked(key provider.RootKey, record *rootRecord, threadID string, node *nodeState) []WaitClassificationDiagnostic {
	grace := o.config.WaitClassification
	if grace <= 0 {
		grace = DefaultWaitClassification
	}
	now := o.config.Now()
	delay := grace
	if state := record.graph.nodes[threadID]; state != nil {
		for _, request := range state.wait.requests {
			if request.owner != requestPending {
				continue
			}
			remaining := grace - elapsedSince(request.startedAt, now)
			if remaining < 0 {
				remaining = 0
			}
			if remaining < delay {
				delay = remaining
			}
		}
	}
	node.wait.classificationToken++
	token := node.wait.classificationToken
	node.wait.classificationPending = true
	node.wait.classificationStarted = now
	// Structured requests own their episode IDs. Allocate a separate episode
	// only for a raw mechanical gate for which Codex supplied no request ID.
	if len(record.graph.pendingRequests(threadID)) == 0 {
		node.wait.classificationEpisode = o.nextWaitEpisodeLocked()
	}
	episode := node.wait.classificationEpisode
	kind := record.graph.classificationKind(threadID)
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
		now := o.config.Now()
		promoted := graph.expireClassification(threadID, now, grace)
		continued := o.reconcileClassificationsLocked(key, current, "timeout_remaining")
		publish := !graph.hasPendingClassification() || graph.hasHumanAttention()
		if publish {
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
		diagnostics := make([]WaitClassificationDiagnostic, 0, len(promoted)+1)
		for _, request := range promoted {
			diagnostics = append(diagnostics, requestDiagnostic("red_published", request, "timeout_fallback", now))
		}
		diagnostics = append(diagnostics, continued...)
		if len(promoted) == 0 && episode != 0 {
			diagnostics = append(diagnostics, classificationDiagnostic(
				"classified", episode, kind, "unknown_timeout", elapsedSince(started, now), false,
			))
		}
		o.emitClassificationDiagnostics(diagnostics)
	})
	if episode == 0 {
		return nil
	}
	return []WaitClassificationDiagnostic{classificationDiagnostic("started", episode, kind, "pending", 0, false)}
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

func (o *Observer) nextWaitEpisodeLocked() uint64 {
	o.waitEpisode++
	return o.waitEpisode
}

func approvalRequestKind(method string) string {
	switch method {
	case "item/commandExecution/requestApproval":
		return "command_execution"
	case "item/fileChange/requestApproval":
		return "file_change"
	case "item/permissions/requestApproval":
		return "permissions"
	case "applyPatchApproval":
		return "legacy_patch"
	case "execCommandApproval":
		return "legacy_command"
	default:
		return "approval"
	}
}

func classifiedRequestDiagnostics(state *graphState, threadID string, pending map[rpcRequestID]requestEvidence, source string, now time.Time) []WaitClassificationDiagnostic {
	var diagnostics []WaitClassificationDiagnostic
	for requestID, prior := range pending {
		request, ok := state.request(threadID, requestID)
		if !ok || request.owner == requestPending {
			continue
		}
		event := "evidence"
		if request.owner == requestHuman && prior.redPublishedAt.IsZero() {
			event = "red_published"
		} else if !prior.redPublishedAt.IsZero() && request.owner == requestAutomatic {
			event = "red_cleared"
		}
		diagnostics = append(diagnostics, requestDiagnostic(event, request, source, now))
	}
	return diagnostics
}

func requestStartDiagnostics(request requestEvidence, now time.Time) []WaitClassificationDiagnostic {
	diagnostics := []WaitClassificationDiagnostic{requestDiagnostic("started", request, "request_received", now)}
	switch request.owner {
	case requestHuman:
		diagnostics = append(diagnostics, requestDiagnostic("red_published", request, request.ownerEvidence, now))
	case requestAutomatic, requestIgnored:
		diagnostics = append(diagnostics, requestDiagnostic("evidence", request, request.ownerEvidence, now))
	}
	return diagnostics
}

func requestDiagnostic(event string, request requestEvidence, source string, now time.Time) WaitClassificationDiagnostic {
	evidence := request.ownerEvidence
	if evidence == "" {
		evidence = "unknown"
	}
	redPublished := !request.redPublishedAt.IsZero()
	humanEvidence := request.owner == requestHuman && evidence != "timeout_fallback"
	diagnostic := WaitClassificationDiagnostic{
		Event: event, Episode: request.episode, RequestKind: request.kind,
		Ownership: requestOwnerLabel(request.owner), Evidence: evidence, Source: source,
		Duration: elapsedSince(request.startedAt, now), RedPublished: redPublished,
		HumanEvidence: humanEvidence,
		ClearedWithoutHumanEvidence: event == "red_cleared" && redPublished && !humanEvidence ||
			(event == "resolved" && redPublished && request.redClearedAt.IsZero() && !humanEvidence),
		SuppressedFalseRed:    request.owner == requestAutomatic && !redPublished,
		LegacyWouldPublishRed: request.ambiguousAtStart && elapsedSince(request.startedAt, now) >= legacyWaitClassification,
	}
	if redPublished {
		redEnd := now
		if !request.redClearedAt.IsZero() {
			redEnd = request.redClearedAt
		}
		diagnostic.RedDuration = elapsedSince(request.redPublishedAt, redEnd)
	}
	return diagnostic
}

func requestOwnerLabel(owner requestOwner) string {
	switch owner {
	case requestHuman:
		return "human"
	case requestAutomatic:
		return "automatic"
	case requestIgnored:
		return "ignored"
	default:
		return "unknown"
	}
}

func classificationDiagnostic(event string, episode uint64, kind, source string, duration time.Duration, suppressed bool) WaitClassificationDiagnostic {
	ownership := "unknown"
	humanEvidence := false
	redPublished := false
	switch source {
	case "reviewer_auto", "auto_review_event", "guardian_source":
		ownership = "automatic"
	case "reviewer_user":
		ownership, humanEvidence, redPublished = "human", true, true
	case "request_timeout", "timeout_fallback":
		ownership, redPublished = "human", true
	}
	return WaitClassificationDiagnostic{
		Event: event, Episode: episode, RequestKind: kind, Ownership: ownership,
		Evidence: source, Source: source, Duration: duration, RedPublished: redPublished,
		HumanEvidence: humanEvidence, SuppressedFalseRed: suppressed,
		LegacyWouldPublishRed: duration >= legacyWaitClassification,
	}
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
