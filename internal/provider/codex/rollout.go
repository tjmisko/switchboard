package codex

// This file is the privacy boundary for Codex rollout ingestion. Rollout JSONL
// contains prompts, responses, tool arguments, paths, and other user content;
// the decoder below admits only identifiers, pricing identity, and numeric usage
// fields. Raw lines, paths, decoder errors, and unknown values are never logged
// or returned to diagnostics.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

const (
	rolloutCursorVersion = 1
	rolloutMaxLineBytes  = 1024 * 1024
	rolloutTurnLimit     = 64

	DiagnosticRolloutBindingInvalid   = "rollout_binding_invalid"
	DiagnosticRolloutOpen             = "rollout_open_error"
	DiagnosticRolloutDecode           = "rollout_decode_error"
	DiagnosticRolloutLineTooLarge     = "rollout_line_too_large"
	DiagnosticRolloutIdentityMismatch = "rollout_identity_mismatch"
	DiagnosticRolloutCursor           = "rollout_cursor_error"
	DiagnosticRolloutPersist          = "rollout_persist_error"
)

var errRolloutRecorderUnavailable = errors.New("codex rollout usage recorder unavailable")

// UsageRecorder is the synchronous durability boundary. PersistUsage must be
// idempotent by UpdateID+Revision and must not return nil until the canonical
// history record is durable. The rollout cursor is never advanced first.
type UsageRecorder interface {
	PersistUsage(context.Context, UsageUpdate) error
}

type VendorUsageRecorder interface {
	PersistVendorUsage(context.Context, UsageUpdate) error
}

type rolloutBinding struct {
	key     provider.RootKey
	rootID  string
	path    string // exact hook path; memory only, never persisted or logged
	fileKey string // path-safe digest persisted in the cursor filename
	boundAt time.Time
}

type rolloutCollector struct {
	mu          sync.Mutex
	stateDir    string
	recorder    UsageRecorder
	diagnostic  func(string)
	now         func() time.Time
	enrich      func(agentgraph.BillingIdentity) agentgraph.BillingIdentity
	onPersisted func(UsageUpdate)
	bindings    map[string]rolloutBinding
}

func newRolloutCollector(stateDir string, recorder UsageRecorder, diagnostic func(string), now func() time.Time) *rolloutCollector {
	if now == nil {
		now = time.Now
	}
	return &rolloutCollector{
		stateDir: stateDir, recorder: recorder, diagnostic: diagnostic, now: now,
		bindings: make(map[string]rolloutBinding),
	}
}

func (c *rolloutCollector) bind(key provider.RootKey, rootID, path string) error {
	rootID = strings.TrimSpace(rootID)
	path = filepath.Clean(strings.TrimSpace(path))
	if key.PID <= 0 || key.StartedAt.IsZero() || rootID == "" || path == "." || !filepath.IsAbs(path) {
		return errors.New("invalid exact rollout binding")
	}
	sum := sha256.Sum256([]byte(rootID + "\x00" + path))
	c.mu.Lock()
	// A session rotation retires every path from the previous root, while
	// multiple exact paths for the same root remain live (child hooks may carry
	// distinct rollout files).
	for bindingKey, existing := range c.bindings {
		if existing.key == key && existing.rootID != rootID {
			delete(c.bindings, bindingKey)
		}
	}
	c.bindings[rolloutBindingKey(key, hex.EncodeToString(sum[:]))] = rolloutBinding{
		key: key, rootID: rootID, path: path,
		fileKey: hex.EncodeToString(sum[:]), boundAt: c.now(),
	}
	c.mu.Unlock()
	return nil
}

func (c *rolloutCollector) forget(key provider.RootKey) {
	c.mu.Lock()
	for bindingKey, binding := range c.bindings {
		if binding.key == key {
			delete(c.bindings, bindingKey)
		}
	}
	c.mu.Unlock()
}

func rolloutBindingKey(key provider.RootKey, fileKey string) string {
	return strconv.Itoa(key.PID) + "\x00" + strconv.FormatInt(key.StartedAt.UnixNano(), 10) + "\x00" + fileKey
}

func (c *rolloutCollector) collectAll(ctx context.Context) {
	c.mu.Lock()
	bindings := make([]rolloutBinding, 0, len(c.bindings))
	for _, binding := range c.bindings {
		bindings = append(bindings, binding)
	}
	c.mu.Unlock()
	for _, binding := range bindings {
		if ctx.Err() != nil {
			return
		}
		c.collect(ctx, binding)
	}
}

type rolloutCursor struct {
	Version       int                                   `json:"version"`
	RootSessionID string                                `json:"root_session_id"`
	FileKey       string                                `json:"file_key"`
	Device        uint64                                `json:"device"`
	Inode         uint64                                `json:"inode"`
	Offset        int64                                 `json:"offset"`
	FileEpoch     uint64                                `json:"file_epoch"`
	Validated     bool                                  `json:"validated"`
	ThreadID      string                                `json:"thread_id,omitempty"`
	ParentThread  string                                `json:"parent_thread_id,omitempty"`
	CurrentTurn   string                                `json:"current_turn_id,omitempty"`
	BaseIdentity  agentgraph.BillingIdentity            `json:"base_identity,omitzero"`
	Turns         map[string]agentgraph.BillingIdentity `json:"turns,omitempty"`
	TurnOrder     []string                              `json:"turn_order,omitempty"`
	Threads       map[string]*rolloutThreadCursor       `json:"threads,omitempty"`
	// ReplayFloor fences already-accounted cumulative prefixes after the exact
	// file is truncated or atomically replaced.
	ReplayFloor map[string]agentgraph.Usage `json:"replay_floor,omitempty"`
	// ReplayMarked makes the conservative replacement/reset ambiguity marker a
	// one-shot durable fact instead of repeating it on every poll below floor.
	ReplayMarked map[string]bool `json:"replay_marked,omitempty"`
}

type rolloutThreadCursor struct {
	Epoch uint64            `json:"epoch"`
	Total agentgraph.Usage  `json:"total,omitzero"`
	Last  *rolloutLastEvent `json:"last,omitempty"`
}

type rolloutLastEvent struct {
	Update UsageUpdate `json:"update"`
}

type rolloutEnvelope struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   rolloutPayload `json:"payload"`
}

// rolloutPayload intentionally contains only content-free, pricing-relevant
// fields. encoding/json scans unknown values but does not retain them.
type rolloutPayload struct {
	Type            string            `json:"type"`
	ID              string            `json:"id"`
	SessionID       string            `json:"session_id"`
	RootSessionID   string            `json:"root_session_id"`
	ThreadID        string            `json:"thread_id"`
	ParentThreadID  string            `json:"parent_thread_id"`
	TurnID          string            `json:"turn_id"`
	ModelProvider   string            `json:"model_provider"`
	Model           string            `json:"model"`
	Effort          string            `json:"effort"`
	ReasoningEffort string            `json:"reasoning_effort"`
	ServiceTier     string            `json:"service_tier"`
	Speed           string            `json:"speed"`
	AuthMode        string            `json:"auth_mode"`
	FromModel       string            `json:"from_model"`
	ToModel         string            `json:"to_model"`
	Info            *rolloutTokenInfo `json:"info"`
	TotalTokenUsage *rolloutUsage     `json:"total_token_usage"`
	LastTokenUsage  *rolloutUsage     `json:"last_token_usage"`
}

type rolloutTokenInfo struct {
	TotalTokenUsage    *rolloutUsage `json:"total_token_usage"`
	LastTokenUsage     *rolloutUsage `json:"last_token_usage"`
	ModelContextWindow *int64        `json:"model_context_window"`
}

type rolloutUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	CacheCreationTokens   int64 `json:"cache_creation_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
	ModelContextWindow    int64 `json:"model_context_window"`
}

func (c *rolloutCollector) collect(ctx context.Context, binding rolloutBinding) {
	if c.recorder == nil {
		c.emit(DiagnosticRolloutPersist)
		return
	}
	f, err := os.Open(binding.path)
	if err != nil {
		c.emit(DiagnosticRolloutOpen)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		c.emit(DiagnosticRolloutOpen)
		return
	}
	device, inode := fileIdentity(info)
	cursor, err := c.loadCursor(binding)
	if err != nil {
		c.emit(DiagnosticRolloutCursor)
		return
	}
	if cursor.Device != device || cursor.Inode != inode || cursor.Offset > info.Size() {
		cursor.resetForFile(binding, device, inode)
	}
	if _, err := f.Seek(cursor.Offset, io.SeekStart); err != nil {
		c.emit(DiagnosticRolloutOpen)
		return
	}
	reader := bufio.NewReaderSize(f, rolloutMaxLineBytes)
	for ctx.Err() == nil {
		line, consumed, complete, oversized, err := readRolloutLine(reader)
		if err != nil {
			c.emit(DiagnosticRolloutOpen)
			return
		}
		if !complete {
			return // a concurrently-written partial JSON value is retried intact
		}
		nextOffset := cursor.Offset + consumed
		if oversized {
			c.emit(DiagnosticRolloutLineTooLarge)
			if !cursor.Validated {
				return
			}
			if err := c.persistCoverageGap(ctx, binding, &cursor, cursor.Offset, nextOffset, "line_too_large"); err != nil {
				c.emit(DiagnosticRolloutPersist)
				return
			}
			if err := c.commitCursor(binding, &cursor, nextOffset); err != nil {
				c.emit(DiagnosticRolloutCursor)
				return
			}
			continue
		}
		var envelope rolloutEnvelope
		if json.Unmarshal(line, &envelope) != nil {
			c.emit(DiagnosticRolloutDecode)
			if !cursor.Validated {
				return
			}
			if err := c.persistCoverageGap(ctx, binding, &cursor, cursor.Offset, nextOffset, "decode_gap"); err != nil {
				c.emit(DiagnosticRolloutPersist)
				return
			}
			if err := c.commitCursor(binding, &cursor, nextOffset); err != nil {
				c.emit(DiagnosticRolloutCursor)
				return
			}
			continue
		}
		update, reject := c.applyEnvelope(binding, &cursor, envelope)
		if reject {
			c.emit(DiagnosticRolloutIdentityMismatch)
			return
		}
		if update != nil {
			update.RootKey = binding.key
			update.RootSessionID = binding.rootID
			if update.ParentThreadID == "" && update.ThreadID != binding.rootID {
				update.ParentThreadID = binding.rootID
			}
			if c.enrich != nil {
				update.Identity = c.enrich(update.Identity)
			}
			if err := c.recorder.PersistUsage(ctx, *update); err != nil {
				c.emit(DiagnosticRolloutPersist)
				return
			}
		}
		if err := c.commitCursor(binding, &cursor, nextOffset); err != nil {
			c.emit(DiagnosticRolloutCursor)
			return
		}
		if update != nil && c.onPersisted != nil {
			c.onPersisted(*update)
		}
	}
}

func readRolloutLine(reader *bufio.Reader) (line []byte, consumed int64, complete, oversized bool, err error) {
	fragment, readErr := reader.ReadSlice('\n')
	consumed += int64(len(fragment))
	if readErr == nil {
		return fragment, consumed, true, false, nil
	}
	if errors.Is(readErr, io.EOF) {
		return nil, 0, false, false, nil
	}
	if !errors.Is(readErr, bufio.ErrBufferFull) {
		return nil, 0, false, false, readErr
	}
	// The record is too large to be a usage/meta record. Discard its remaining
	// bytes without retaining content, then durably advance past the whole line.
	for {
		fragment, readErr = reader.ReadSlice('\n')
		consumed += int64(len(fragment))
		switch {
		case readErr == nil:
			return nil, consumed, true, true, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return nil, 0, false, true, nil
		default:
			return nil, 0, false, true, readErr
		}
	}
}

func (c *rolloutCollector) persistCoverageGap(ctx context.Context, binding rolloutBinding, cursor *rolloutCursor, start, end int64, reason string) error {
	threadID := firstRolloutValue(cursor.ThreadID, binding.rootID)
	total := agentgraph.Usage{}
	if thread := cursor.Threads[threadID]; thread != nil {
		total = thread.Total
	}
	identity := mergeBilling(cursor.BaseIdentity, cursor.Turns[turnKey(threadID, cursor.CurrentTurn)])
	identity.AgentClient = string(agentgraph.ProviderCodex)
	if c.enrich != nil {
		identity = c.enrich(identity)
	}
	update := UsageUpdate{
		RootKey: binding.key, RootSessionID: binding.rootID, ThreadID: threadID,
		ParentThreadID: cursor.ParentThread, TurnID: cursor.CurrentTurn,
		UpdateID: coverageGapID(binding.rootID, binding.fileKey, start, end, reason), Revision: 1,
		Reconciliation: reason, Coverage: "partial", Source: agentgraph.SourceCodexRollout,
		Identity: identity, Total: total, ObservedAt: c.now(),
	}
	return c.recorder.PersistUsage(ctx, update)
}

func (c *rolloutCollector) applyEnvelope(binding rolloutBinding, cursor *rolloutCursor, envelope rolloutEnvelope) (*UsageUpdate, bool) {
	payload := envelope.Payload
	eventType := strings.TrimSpace(envelope.Type)
	payloadType := strings.TrimSpace(payload.Type)
	switch {
	case eventType == "session_meta":
		threadID := firstRolloutValue(payload.ID, payload.ThreadID, payload.SessionID)
		rootID := firstRolloutValue(payload.RootSessionID, payload.ParentThreadID)
		if threadID == "" || (threadID != binding.rootID && rootID != binding.rootID) {
			return nil, true
		}
		cursor.Validated = true
		cursor.ThreadID = threadID
		if threadID != binding.rootID {
			cursor.ParentThread = binding.rootID
		}
		cursor.BaseIdentity = mergeBilling(cursor.BaseIdentity, identityFromPayload(payload))
		return c.reviseLatest(cursor, threadID, ""), false
	case eventType == "turn_context":
		if !cursor.Validated {
			return nil, false
		}
		threadID := firstRolloutValue(payload.ThreadID, cursor.ThreadID, binding.rootID)
		turnID := strings.TrimSpace(payload.TurnID)
		if turnID == "" {
			return nil, false
		}
		cursor.ThreadID, cursor.CurrentTurn = threadID, turnID
		identity := mergeBilling(cursor.BaseIdentity, identityFromPayload(payload))
		cursor.rememberTurn(turnKey(threadID, turnID), identity)
		return c.reviseLatest(cursor, threadID, turnID), false
	case eventType == "model_rerouted" || eventType == "model_reroute" ||
		payloadType == "model_rerouted" || payloadType == "model_reroute":
		if !cursor.Validated {
			return nil, false
		}
		threadID := firstRolloutValue(payload.ThreadID, cursor.ThreadID, binding.rootID)
		turnID := firstRolloutValue(payload.TurnID, cursor.CurrentTurn)
		key := turnKey(threadID, turnID)
		identity := mergeBilling(cursor.BaseIdentity, cursor.Turns[key])
		if to := strings.TrimSpace(payload.ToModel); to != "" {
			identity.Model = to
		}
		identity = mergeBilling(identity, identityFromPayload(payload))
		cursor.rememberTurn(key, identity)
		update := c.reviseLatest(cursor, threadID, turnID)
		if update != nil {
			update.ReroutedFromModel = strings.TrimSpace(payload.FromModel)
		}
		return update, false
	case eventType == "event_msg" && payloadType == "token_count", eventType == "token_count":
		if !cursor.Validated {
			return nil, false
		}
		return c.applyTokenCount(binding, cursor, envelope), false
	default:
		return nil, false
	}
}

func (c *rolloutCollector) applyTokenCount(binding rolloutBinding, cursor *rolloutCursor, envelope rolloutEnvelope) *UsageUpdate {
	payload := envelope.Payload
	threadID := firstRolloutValue(payload.ThreadID, cursor.ThreadID, binding.rootID)
	turnID := firstRolloutValue(payload.TurnID, cursor.CurrentTurn)
	if cursor.Threads == nil {
		cursor.Threads = make(map[string]*rolloutThreadCursor)
	}
	thread := cursor.Threads[threadID]
	if thread == nil {
		thread = &rolloutThreadCursor{}
		cursor.Threads[threadID] = thread
	}
	totalRaw, lastRaw, contextWindow := payload.usage()
	if totalRaw == nil && lastRaw == nil {
		return nil
	}
	if payloadIdentity := identityFromPayload(payload); !payloadIdentity.IsZero() {
		key := turnKey(threadID, turnID)
		cursor.rememberTurn(key, mergeBilling(cursor.Turns[key], payloadIdentity))
	}
	var total, delta agentgraph.Usage
	status := "delta"
	coverage := ""
	if totalRaw != nil {
		total = totalRaw.agentUsage(contextWindow)
		if !validAgentUsage(total) {
			return nil
		}
		if floor, replaying := cursor.ReplayFloor[threadID]; replaying {
			switch {
			case usageTupleEqual(total, floor):
				delete(cursor.ReplayFloor, threadID)
				delete(cursor.ReplayMarked, threadID)
				return nil
			case usageTupleLessOrEqual(total, floor):
				// This may be a replayed prefix, or it may be a real counter
				// reset that never reaches the prior floor. Preserve the prior
				// total and durably disclose the ambiguity once.
				if cursor.ReplayMarked[threadID] {
					return nil
				}
				cursor.ReplayMarked[threadID] = true
				thread.Total = floor
				total = floor
				status, coverage = "replacement_replay_ambiguity", "partial"
			case usageMonotonic(total, floor):
				thread.Total = floor
				delete(cursor.ReplayFloor, threadID)
				delete(cursor.ReplayMarked, threadID)
			default:
				// A mixed tuple after physical replacement is ambiguous. Reporting
				// partial is safer than manufacturing a second counter epoch.
				cursor.ReplayMarked[threadID] = true
				thread.Total = floor
				total = floor
				status, coverage = "replacement_discontinuity", "partial"
			}
		}
		if coverage == "" {
			switch {
			case usageTupleEqual(total, thread.Total):
				contextChanged := false
				if total.ModelContextWindow != thread.Total.ModelContextWindow {
					thread.Total.ModelContextWindow = total.ModelContextWindow
					if thread.Last != nil {
						thread.Last.Update.Total.ModelContextWindow = total.ModelContextWindow
						thread.Last.Update.Delta.ModelContextWindow = total.ModelContextWindow
					}
					contextChanged = true
				}
				if revised := c.reviseLatest(cursor, threadID, turnID); revised != nil {
					return revised
				}
				if contextChanged && thread.Last != nil {
					thread.Last.Update.Revision++
					thread.Last.Update.Reconciliation = "metadata_revision"
					return &thread.Last.Update
				}
				return nil
			case usageMonotonic(total, thread.Total):
				var ok bool
				delta, ok = subtractUsage(total, thread.Total)
				if !ok {
					return nil
				}
				firstAttach := usageTokensZero(thread.Total)
				if firstAttach {
					status = "first_attach"
				} else if lastRaw != nil {
					last := lastRaw.agentUsage(contextWindow)
					if !validAgentUsage(last) || !usageTupleEqual(last, delta) {
						status, coverage = "last_total_gap", "partial"
						if validAgentUsage(last) && usageTupleLessOrEqual(last, delta) {
							delta = last
						} else {
							delta = agentgraph.Usage{ModelContextWindow: total.ModelContextWindow}
						}
					}
				}
			default:
				// Any partial regression starts a new counter epoch. Capturing the new
				// tuple from zero is conservative and never subtracts prior spend.
				thread.Epoch++
				delta = total
				status, coverage = "counter_epoch", "partial"
			}
		}
	} else {
		if _, replaying := cursor.ReplayFloor[threadID]; replaying {
			return nil
		}
		delta = lastRaw.agentUsage(contextWindow)
		if !validAgentUsage(delta) || usageTokensZero(delta) {
			return nil
		}
		var ok bool
		total, ok = addUsage(thread.Total, delta)
		if !ok {
			return nil
		}
		status = "last_only"
	}
	if usageTokensZero(delta) && coverage == "" {
		return nil
	}
	identity := mergeBilling(cursor.BaseIdentity, cursor.Turns[turnKey(threadID, turnID)])
	identity.AgentClient = string(agentgraph.ProviderCodex)
	observedAt := c.now()
	if parsed, err := time.Parse(time.RFC3339Nano, envelope.Timestamp); err == nil {
		observedAt = parsed
	}
	updateID := rolloutUpdateID(binding.rootID, threadID, thread.Epoch, total)
	if usageTokensZero(delta) {
		updateID = coverageTupleID(binding.rootID, threadID, thread.Epoch, total, status)
	}
	update := UsageUpdate{
		RootKey: binding.key, RootSessionID: binding.rootID,
		ThreadID: threadID, ParentThreadID: cursor.ParentThread, TurnID: turnID,
		UpdateID: updateID,
		Revision: 1, Reconciliation: status, Coverage: coverage, Source: agentgraph.SourceCodexRollout,
		Identity: identity, Delta: delta, Total: total, ObservedAt: observedAt,
	}
	thread.Total = total
	thread.Last = &rolloutLastEvent{Update: update}
	return &thread.Last.Update
}

func (c *rolloutCollector) reviseLatest(cursor *rolloutCursor, threadID, turnID string) *UsageUpdate {
	thread := cursor.Threads[threadID]
	if thread == nil || thread.Last == nil {
		return nil
	}
	last := &thread.Last.Update
	if turnID != "" && last.TurnID != turnID {
		return nil
	}
	identity := mergeBilling(cursor.BaseIdentity, cursor.Turns[turnKey(threadID, last.TurnID)])
	identity.AgentClient = string(agentgraph.ProviderCodex)
	if identity == last.Identity {
		return nil
	}
	last.Identity = identity
	last.Revision++
	last.Reconciliation = "metadata_revision"
	return last
}

func (p rolloutPayload) usage() (total, last *rolloutUsage, contextWindow *int64) {
	total, last = p.TotalTokenUsage, p.LastTokenUsage
	if p.Info != nil {
		if p.Info.TotalTokenUsage != nil {
			total = p.Info.TotalTokenUsage
		}
		if p.Info.LastTokenUsage != nil {
			last = p.Info.LastTokenUsage
		}
		contextWindow = p.Info.ModelContextWindow
	}
	return total, last, contextWindow
}

func (u rolloutUsage) agentUsage(contextWindow *int64) agentgraph.Usage {
	cacheWrite := u.CacheWriteInputTokens
	if cacheWrite == 0 {
		cacheWrite = u.CacheCreationTokens
	}
	out := agentgraph.Usage{
		InputTokens: u.InputTokens, CachedInputTokens: u.CachedInputTokens,
		CacheWriteInputTokens: cacheWrite, OutputTokens: u.OutputTokens,
		ReasoningOutputTokens: u.ReasoningOutputTokens, TotalTokens: u.TotalTokens,
		ModelContextWindow: u.ModelContextWindow,
	}
	if contextWindow != nil {
		out.ModelContextWindow = *contextWindow
	}
	return out
}

func identityFromPayload(p rolloutPayload) agentgraph.BillingIdentity {
	effort := firstRolloutValue(p.ReasoningEffort, p.Effort)
	return agentgraph.BillingIdentity{
		AgentClient: string(agentgraph.ProviderCodex), ExecutionProvider: strings.TrimSpace(p.ModelProvider),
		AuthMode: strings.TrimSpace(p.AuthMode), Model: strings.TrimSpace(p.Model),
		ServiceTier: strings.TrimSpace(p.ServiceTier), Speed: strings.TrimSpace(p.Speed),
		ReasoningEffort: effort,
	}
}

func mergeBilling(base, update agentgraph.BillingIdentity) agentgraph.BillingIdentity {
	if update.AgentClient != "" {
		base.AgentClient = update.AgentClient
	}
	if update.ExecutionProvider != "" {
		base.ExecutionProvider = update.ExecutionProvider
	}
	if update.AuthMode != "" {
		base.AuthMode = update.AuthMode
	}
	if update.BillingRoute != "" {
		base.BillingRoute = update.BillingRoute
	}
	if update.AccountKind != "" {
		base.AccountKind = update.AccountKind
	}
	if update.Model != "" {
		base.Model = update.Model
	}
	if update.ServiceTier != "" {
		base.ServiceTier = update.ServiceTier
	}
	if update.Speed != "" {
		base.Speed = update.Speed
	}
	if update.InferenceGeo != "" {
		base.InferenceGeo = update.InferenceGeo
	}
	if update.ReasoningEffort != "" {
		base.ReasoningEffort = update.ReasoningEffort
	}
	return base
}

func (c *rolloutCursor) rememberTurn(key string, identity agentgraph.BillingIdentity) {
	if key == "\x00" {
		return
	}
	if c.Turns == nil {
		c.Turns = make(map[string]agentgraph.BillingIdentity)
	}
	if _, exists := c.Turns[key]; !exists {
		c.TurnOrder = append(c.TurnOrder, key)
	}
	c.Turns[key] = identity
	for len(c.TurnOrder) > rolloutTurnLimit {
		oldest := c.TurnOrder[0]
		c.TurnOrder = c.TurnOrder[1:]
		delete(c.Turns, oldest)
	}
}

func turnKey(threadID, turnID string) string { return threadID + "\x00" + turnID }

func firstRolloutValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func validAgentUsage(usage agentgraph.Usage) bool {
	return usage.InputTokens >= 0 && usage.CachedInputTokens >= 0 && usage.CacheWriteInputTokens >= 0 &&
		usage.OutputTokens >= 0 && usage.ReasoningOutputTokens >= 0 && usage.TotalTokens >= 0 &&
		usage.ModelContextWindow >= 0
}

func usageTupleEqual(left, right agentgraph.Usage) bool {
	left.ModelContextWindow, right.ModelContextWindow = 0, 0
	return left == right
}

func usageMonotonic(current, previous agentgraph.Usage) bool {
	return current.InputTokens >= previous.InputTokens && current.CachedInputTokens >= previous.CachedInputTokens &&
		current.CacheWriteInputTokens >= previous.CacheWriteInputTokens && current.OutputTokens >= previous.OutputTokens &&
		current.ReasoningOutputTokens >= previous.ReasoningOutputTokens && current.TotalTokens >= previous.TotalTokens
}

func usageTupleLessOrEqual(current, previous agentgraph.Usage) bool {
	return current.InputTokens <= previous.InputTokens && current.CachedInputTokens <= previous.CachedInputTokens &&
		current.CacheWriteInputTokens <= previous.CacheWriteInputTokens && current.OutputTokens <= previous.OutputTokens &&
		current.ReasoningOutputTokens <= previous.ReasoningOutputTokens && current.TotalTokens <= previous.TotalTokens
}

func rolloutUpdateID(rootID, threadID string, counterEpoch uint64, total agentgraph.Usage) string {
	h := sha256.New()
	for _, value := range []string{
		"codex-rollout-v2", rootID, threadID, strconv.FormatUint(counterEpoch, 10),
		strconv.FormatInt(total.InputTokens, 10), strconv.FormatInt(total.CachedInputTokens, 10),
		strconv.FormatInt(total.CacheWriteInputTokens, 10), strconv.FormatInt(total.OutputTokens, 10),
		strconv.FormatInt(total.ReasoningOutputTokens, 10), strconv.FormatInt(total.TotalTokens, 10),
	} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func coverageGapID(rootID, fileKey string, start, end int64, reason string) string {
	return digestStable(struct {
		Kind    string `json:"kind"`
		RootID  string `json:"root_id"`
		FileKey string `json:"file_key"`
		Start   int64  `json:"start"`
		End     int64  `json:"end"`
		Reason  string `json:"reason"`
	}{Kind: "codex_rollout_coverage_gap_v1", RootID: rootID, FileKey: fileKey, Start: start, End: end, Reason: reason})
}

func coverageTupleID(rootID, threadID string, counterEpoch uint64, total agentgraph.Usage, reason string) string {
	return digestStable(struct {
		Kind         string           `json:"kind"`
		RootID       string           `json:"root_id"`
		ThreadID     string           `json:"thread_id"`
		CounterEpoch uint64           `json:"counter_epoch"`
		Total        agentgraph.Usage `json:"total"`
		Reason       string           `json:"reason"`
	}{Kind: "codex_rollout_coverage_tuple_v1", RootID: rootID, ThreadID: threadID, CounterEpoch: counterEpoch, Total: total, Reason: reason})
}

func fileIdentity(info os.FileInfo) (device, inode uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), uint64(stat.Ino)
	}
	return 0, uint64(info.ModTime().UnixNano()) ^ uint64(info.Size())
}

func (c *rolloutCursor) resetForFile(binding rolloutBinding, device, inode uint64) {
	nextEpoch := c.FileEpoch + 1
	replayFloor := make(map[string]agentgraph.Usage, len(c.Threads))
	for threadID, thread := range c.Threads {
		if thread != nil && !usageTokensZero(thread.Total) {
			replayFloor[threadID] = thread.Total
		}
	}
	turns := c.Turns
	if turns == nil {
		turns = make(map[string]agentgraph.BillingIdentity)
	}
	threads := c.Threads
	if threads == nil {
		threads = make(map[string]*rolloutThreadCursor)
	}
	*c = rolloutCursor{
		Version: rolloutCursorVersion, RootSessionID: binding.rootID, FileKey: binding.fileKey,
		Device: device, Inode: inode, FileEpoch: nextEpoch,
		ThreadID: c.ThreadID, ParentThread: c.ParentThread, CurrentTurn: c.CurrentTurn,
		BaseIdentity: c.BaseIdentity, Turns: turns, TurnOrder: c.TurnOrder, Threads: threads,
		ReplayFloor: replayFloor, ReplayMarked: make(map[string]bool),
	}
}

func (c *rolloutCollector) loadCursor(binding rolloutBinding) (rolloutCursor, error) {
	cursor := rolloutCursor{
		Version: rolloutCursorVersion, RootSessionID: binding.rootID, FileKey: binding.fileKey,
		Turns: make(map[string]agentgraph.BillingIdentity), Threads: make(map[string]*rolloutThreadCursor),
	}
	if c.stateDir == "" {
		return cursor, errRolloutRecorderUnavailable
	}
	body, err := os.ReadFile(c.cursorPath(binding))
	if errors.Is(err, os.ErrNotExist) {
		return cursor, nil
	}
	if err != nil {
		return cursor, err
	}
	if json.Unmarshal(body, &cursor) != nil || cursor.Version != rolloutCursorVersion ||
		cursor.RootSessionID != binding.rootID || cursor.FileKey != binding.fileKey || cursor.Offset < 0 {
		return rolloutCursor{}, errors.New("invalid rollout cursor")
	}
	if cursor.Turns == nil {
		cursor.Turns = make(map[string]agentgraph.BillingIdentity)
	}
	if cursor.Threads == nil {
		cursor.Threads = make(map[string]*rolloutThreadCursor)
	}
	if cursor.ReplayFloor == nil {
		cursor.ReplayFloor = make(map[string]agentgraph.Usage)
	}
	if cursor.ReplayMarked == nil {
		cursor.ReplayMarked = make(map[string]bool)
	}
	return cursor, nil
}

func (c *rolloutCollector) commitCursor(binding rolloutBinding, cursor *rolloutCursor, offset int64) error {
	cursor.Offset = offset
	cursor.Version = rolloutCursorVersion
	cursor.RootSessionID = binding.rootID
	cursor.FileKey = binding.fileKey
	body, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.stateDir, ".cursor-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if n, err := tmp.Write(body); err != nil {
		return err
	} else if n != len(body) {
		return io.ErrShortWrite
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, c.cursorPath(binding)); err != nil {
		return err
	}
	dir, err := os.Open(c.stateDir)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func (c *rolloutCollector) cursorPath(binding rolloutBinding) string {
	return filepath.Join(c.stateDir, binding.fileKey+".json")
}

func (c *rolloutCollector) emit(category string) {
	if c.diagnostic != nil {
		c.diagnostic(category)
	}
}

func (c rolloutCursor) String() string {
	// Deliberately content/path-free for accidental formatting in tests/errors.
	return fmt.Sprintf("rollout-cursor{offset=%d,validated=%t,epoch=%d}", c.Offset, c.Validated, c.FileEpoch)
}
