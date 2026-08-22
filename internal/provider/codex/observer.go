// Package codex observes Codex app-server state through the read-only
// `codex app-server proxy` surface and projects it onto the neutral agent graph.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

const (
	DefaultFreshness        = 15 * time.Second
	DefaultActiveResnapshot = 1 * time.Second
	DefaultIdleResnapshot   = 10 * time.Second
	DefaultRequestTimeout   = 5 * time.Second
	DefaultReconnectMinimum = 100 * time.Millisecond
	DefaultReconnectMaximum = 5 * time.Second
	DefaultTerminalLimit    = 32
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
	EndpointConnector   func(string) Connector
	Environment         EnvironmentReader
	Freshness           time.Duration
	ResnapshotInterval  time.Duration
	ActivePollInterval  time.Duration
	IdlePollInterval    time.Duration
	RequestTimeout      time.Duration
	ReconnectMinimum    time.Duration
	ReconnectMaximum    time.Duration
	UpdateBuffer        int
	RecentTerminalLimit int
	Now                 func() time.Time
	Jitter              func(time.Duration) time.Duration
	Diagnostic          func(string)
}

type rootRecord struct {
	threadID          string
	binding           BindingSource
	bindingGeneration uint64
	graph             *graphState
	observation       agentgraph.Observation
	generation        uint64
	expiry            *time.Timer
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
	closed          bool
	lastDiagnostic  time.Time
	lastError       string

	refresh chan struct{}
	cadence chan struct{}
	control chan nameSetRequest
}

type nameSetRequest struct {
	ctx      context.Context
	key      provider.RootKey
	threadID string
	name     string
	done     chan error
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
		refresh: make(chan struct{}, 1), cadence: make(chan struct{}, 1), control: make(chan nameSetRequest),
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

// RegisterHookBinding records a daemon-verified exact thread ID. Environment
// CODEX_THREAD_ID still has precedence. Registration triggers a resnapshot and
// is safe to call before or after the first Observe.
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
func (o *Observer) Observe(ctx context.Context, ref provider.RootRef, _ time.Time) (agentgraph.Observation, error) {
	if ref.Provider != "" && ref.Provider != agentgraph.ProviderCodex {
		return agentgraph.Observation{}, fmt.Errorf("codex observer cannot observe provider %q", ref.Provider)
	}
	if ref.PID <= 0 || ref.StartedAt.IsZero() {
		return agentgraph.Observation{Provider: agentgraph.ProviderCodex, Complete: false, Diagnostic: "process start identity unavailable"}, nil
	}
	binding, diagnostic := o.bindings.resolve(ctx, ref)
	if binding.ThreadID == "" {
		// A launcher-owned endpoint can discover its current root without guessing
		// by cwd or process ancestry. The supervisor reconciles loaded threads.
		if ref.SlotID != "" && ref.ProviderEndpoint != "" {
			key := ref.Key()
			o.mu.Lock()
			if o.roots[key] == nil {
				o.roots[key] = &rootRecord{}
			}
			o.mu.Unlock()
			o.signalRefresh()
		}
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
		record = &rootRecord{threadID: binding.ThreadID, binding: binding.Source, bindingGeneration: binding.Generation}
		o.roots[key] = record
		changed = true
	} else if record.threadID != binding.ThreadID || record.bindingGeneration != binding.Generation {
		if record.expiry != nil {
			record.expiry.Stop()
		}
		record = &rootRecord{threadID: binding.ThreadID, binding: binding.Source, bindingGeneration: binding.Generation}
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

// SetThreadName is the observer's sole intentional write. It is serialized on
// the connection supervisor and gated by the current thread binding.
func (o *Observer) SetThreadName(ctx context.Context, key provider.RootKey, threadID, name string) error {
	request := nameSetRequest{ctx: ctx, key: key, threadID: strings.TrimSpace(threadID), name: strings.TrimSpace(name), done: make(chan error, 1)}
	if request.threadID == "" || request.name == "" {
		return errors.New("codex: thread name requires thread id and name")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.ctx.Done():
		return errors.New("codex observer is closed")
	case o.control <- request:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-request.done:
		return err
	}
}

// Forget drops all binding, graph, and timer state for one process lifetime.
// It is idempotent and does not affect another process that reused the PID.
func (o *Observer) Forget(key provider.RootKey) {
	o.bindings.Forget(key)
	o.mu.Lock()
	if record := o.roots[key]; record != nil && record.expiry != nil {
		record.expiry.Stop()
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
			o.recordError("endpoint_connect_error")
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
		o.mu.Unlock()

		client := newRPCClient(connection, generation, o.handleNotification)
		if err := o.initialize(client); err != nil {
			o.recordError("endpoint_initialize_error")
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
			case request := <-o.control:
				o.mu.Lock()
				record := o.roots[request.key]
				valid := record != nil && record.threadID == request.threadID && o.generation == generation
				o.mu.Unlock()
				if !valid {
					request.done <- errors.New("codex: stale thread name request")
					continue
				}
				ctx, cancel := context.WithTimeout(request.ctx, o.config.RequestTimeout)
				err := client.request(ctx, "thread/name/set", map[string]any{"threadId": request.threadID, "name": request.name}, &struct{}{})
				cancel()
				request.done <- err
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
		// Per-TUI endpoints reconcile their loaded root on every poll. This heals
		// missed SessionStart hooks, /clear, and app-server restarts.
		if target.key.SlotID != "" || target.id == "" {
			discovered, err := o.discoverLoadedRoot(client)
			if err != nil {
				o.recordError("loaded_thread_reconcile_error")
				continue
			}
			if discovered != "" && discovered != target.id {
				update, bindErr := o.bindings.RegisterHook(target.key, discovered)
				if bindErr != nil || update.Stale {
					continue
				}
				o.mu.Lock()
				record := o.roots[target.key]
				if o.generation != generation || record == nil || record.threadID != target.id {
					o.mu.Unlock()
					continue
				}
				if record.expiry != nil {
					record.expiry.Stop()
				}
				record.threadID = discovered
				record.binding = BindingHook
				record.bindingGeneration = update.Generation
				record.graph = nil
				record.observation = agentgraph.Observation{}
				o.mu.Unlock()
				target.id = discovered
			}
		}
		if target.id == "" {
			continue
		}
		state, err := o.snapshot(client, target.id)
		if err != nil {
			o.recordError("thread_snapshot_error")
			continue
		}
		o.installSnapshot(generation, target.key, target.id, state)
	}
}

func (o *Observer) recordError(category string) {
	o.mu.Lock()
	o.lastError = category
	o.mu.Unlock()
}

func (o *Observer) discoverLoadedRoot(client *rpcClient) (string, error) {
	ctx, cancel := context.WithTimeout(o.ctx, o.config.RequestTimeout)
	defer cancel()
	var candidates []rpcThread
	var cursor string
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var loaded threadLoadedListResult
		if err := client.request(ctx, "thread/loaded/list", params, &loaded); err != nil {
			return "", err
		}
		for _, id := range loaded.Data {
			var result threadReadResult
			if err := client.request(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &result); err != nil {
				continue
			}
			if result.Thread.ID == id && result.Thread.ParentThreadID == "" {
				candidates = append(candidates, result.Thread)
			}
		}
		if loaded.NextCursor == nil || *loaded.NextCursor == "" {
			break
		}
		cursor = *loaded.NextCursor
	}
	if len(candidates) == 0 {
		return "", nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt != candidates[j].CreatedAt {
			return candidates[i].CreatedAt > candidates[j].CreatedAt
		}
		if candidates[i].UpdatedAt != candidates[j].UpdatedAt {
			return candidates[i].UpdatedAt > candidates[j].UpdatedAt
		}
		return candidates[i].ID > candidates[j].ID
	})
	return candidates[0].ID, nil
}

func (o *Observer) snapshot(client *rpcClient, rootID string) (*graphState, error) {
	ctx, cancel := context.WithTimeout(o.ctx, o.config.RequestTimeout)
	defer cancel()
	var rootResult threadReadResult
	if err := client.request(ctx, "thread/read", map[string]any{"threadId": rootID, "includeTurns": true}, &rootResult); err != nil {
		return nil, err
	}
	if rootResult.Thread.ID != rootID {
		return nil, errors.New("codex app-server returned a different root thread")
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
			return nil, err
		}
		descendants = append(descendants, result.Data...)
		if result.NextCursor == nil || *result.NextCursor == "" {
			break
		}
		cursor = *result.NextCursor
	}
	state := newGraphState(rootResult.Thread, descendants, o.config.RecentTerminalLimit)
	if _, err := state.observation(o.config.Now(), o.config.Freshness); err != nil {
		return nil, err
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
	record.graph = state
	record.observation = observation
	record.generation = generation
	o.scheduleExpiryLocked(key, record)
	o.mu.Unlock()
	o.queue.Signal(key)
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
	keys := make([]provider.RootKey, 0, len(o.roots))
	for key := range o.roots {
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
		notification.Params = append(json.RawMessage(nil), notification.Params...)
		o.queued = append(o.queued, notification)
		o.mu.Unlock()
		return
	}
	keys, unknown := o.applyNotificationLocked(notification)
	o.mu.Unlock()
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
}

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

func (o *Observer) applyNotificationLocked(notification rpcNotification) ([]provider.RootKey, bool) {
	type threadParams struct {
		ThreadID   string          `json:"threadId"`
		ThreadName *string         `json:"threadName"`
		TurnID     string          `json:"turnId"`
		ItemID     string          `json:"itemId"`
		RequestID  json.RawMessage `json:"requestId"`
		Thread     rpcThread       `json:"thread"`
		Status     rpcStatus       `json:"status"`
		Turn       rpcTurn         `json:"turn"`
		Item       rpcItem         `json:"item"`
	}
	params, ok := decodeParams[threadParams](notification.Params)
	if !ok {
		return nil, false
	}
	var changed []provider.RootKey
	unknown := false
	statusMatched := false
	for key, record := range o.roots {
		if record.graph == nil || record.generation != notification.Generation {
			continue
		}
		state := record.graph
		touches := false
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
				node.node.Runtime = agentgraph.RuntimeActive
				touches = true
			}
		case "turn/completed":
			if state.nodes[params.ThreadID] != nil {
				state.completeTurn(params.ThreadID, params.Turn.ID)
				touches = true
			}
		case "item/started", "item/completed":
			if state.nodes[params.ThreadID] != nil || state.nodes[params.Item.SenderThreadID] != nil {
				state.applyItem(params.ThreadID, params.TurnID, params.Item)
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
		case "item/tool/requestUserInput":
			if state.nodes[params.ThreadID] != nil && params.ItemID != "" {
				state.beginUserInputRequest(params.ThreadID, params.TurnID, params.ItemID, notification.RequestID)
				touches = true
			}
		case "serverRequest/resolved":
			if state.nodes[params.ThreadID] != nil {
				state.resolveUserInputRequest(params.ThreadID, strings.TrimSpace(string(params.RequestID)))
				touches = true
			}
		case "thread/archived", "thread/deleted":
			if state.nodes[params.ThreadID] != nil && params.ThreadID != state.rootID {
				state.deleteThread(params.ThreadID)
				touches = true
			}
		}
		if !touches {
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
	return changed, unknown
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
	if !state.unknownEnum || o.config.Diagnostic == nil {
		return
	}
	o.mu.Lock()
	now := o.config.Now()
	if !o.lastDiagnostic.IsZero() && now.Sub(o.lastDiagnostic) < time.Minute {
		o.mu.Unlock()
		return
	}
	o.lastDiagnostic = now
	o.mu.Unlock()
	o.config.Diagnostic("codex observer received an unknown protocol enum")
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
