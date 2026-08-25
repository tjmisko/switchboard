package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	usageCursorVersion         = 1
	usageCursorFilename        = "claude-usage-cursors-v1.json"
	maxUsageSourcesPerSession  = 4096
	maxUsageMessagesPerSession = 32768
	maxUsageSessions           = 1024
	usageGenerationPrefixBytes = 4096
)

// UsageDelta is a positive, exactly-once change to a logical Claude provider
// message. A later streamed fragment can only increase its counters. SourceID
// is an opaque transcript identity (root or a relative child-agent id), never a
// filesystem path.
type UsageDelta struct {
	ProviderMessageID string
	SourceID          string
	Timestamp         time.Time
	Model             string
	Usage             Usage
}

type usageTrackerState struct {
	Version  int                           `json:"version"`
	Sessions map[string]*usageSessionState `json:"sessions"`
}

type usageSessionState struct {
	LastSeen     time.Time                    `json:"last_seen"`
	Sources      map[string]usageSourceCursor `json:"sources"`
	Messages     map[string]usageMessageState `json:"messages"`
	MessageOrder []string                     `json:"message_order"`
}

type usageSourceCursor struct {
	Offset     int64  `json:"offset"`
	Generation string `json:"generation"`
}

type usageMessageState struct {
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	SourceID          string    `json:"source_id"`
	Timestamp         time.Time `json:"timestamp,omitempty"`
	Model             string    `json:"model,omitempty"`
	Usage             Usage     `json:"usage"`
}

// UsageTracker owns restart-safe Claude transcript cursors and the monotonic
// maximum seen for each provider message. Its state file contains only opaque
// session/message/source ids, pricing dimensions, counters, and timestamps; it
// never persists transcript paths or conversation content.
type UsageTracker struct {
	mu        sync.Mutex
	stateDir  string
	statePath string
	state     usageTrackerState
}

// NewUsageTracker loads (or creates in memory) the durable cursor under
// stateDir. An existing malformed or unknown-version file is an error: silently
// starting over would replay a whole transcript and corrupt cost totals.
func NewUsageTracker(stateDir string) (*UsageTracker, error) {
	if stateDir == "" {
		return nil, errors.New("transcript: usage cursor state directory is required")
	}
	t := &UsageTracker{
		stateDir: stateDir, statePath: filepath.Join(stateDir, usageCursorFilename),
		state: usageTrackerState{Version: usageCursorVersion, Sessions: map[string]*usageSessionState{}},
	}
	raw, err := os.ReadFile(t.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load Claude usage cursor: %w", err)
	}
	var state usageTrackerState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode Claude usage cursor: %w", err)
	}
	if state.Version != usageCursorVersion {
		return nil, fmt.Errorf("decode Claude usage cursor: unsupported version %d", state.Version)
	}
	if state.Sessions == nil {
		state.Sessions = map[string]*usageSessionState{}
	}
	for _, session := range state.Sessions {
		normalizeUsageSessionState(session)
	}
	t.state = state
	return t, nil
}

// ObserveSession backfills then incrementally reads the root transcript and all
// root-owned child transcripts, returning positive per-message deltas. State is
// atomically persisted before deltas are returned; if persistence fails, no
// in-memory cursor advances and the caller must not emit the returned work.
func (t *UsageTracker) ObserveSession(sessionID, rootTranscript string, observedAt time.Time) ([]UsageDelta, error) {
	if sessionID == "" || rootTranscript == "" {
		return nil, errors.New("transcript: session id and root transcript are required")
	}
	sources, err := usageSources(rootTranscript)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	priorSession := t.state.Sessions[sessionID]
	session := cloneUsageSessionState(priorSession)
	if session == nil {
		session = newUsageSessionState()
	}
	changed := priorSession == nil
	type sourcedRecord struct {
		UsageRecord
		sourceID string
	}
	var records []sourcedRecord

	for _, source := range sources {
		generation, size, err := usageSourceGeneration(source.path)
		if err != nil {
			return nil, fmt.Errorf("read Claude usage source %s: %w", source.id, err)
		}
		cursor, existed := session.Sources[source.id]
		if !existed || cursor.Generation != generation || cursor.Offset > size {
			cursor.Offset = 0
			cursor.Generation = generation
			changed = true
		}
		parsed, newOffset, err := UsageRecordsSince(source.path, cursor.Offset)
		if err != nil {
			return nil, fmt.Errorf("read Claude usage source %s: %w", source.id, err)
		}
		if newOffset != cursor.Offset {
			cursor.Offset = newOffset
			changed = true
		}
		session.Sources[source.id] = cursor
		for _, record := range parsed {
			records = append(records, sourcedRecord{UsageRecord: record, sourceID: source.id})
		}
	}

	// Collapse all sources as one session-wide message namespace. This catches a
	// provider message copied into both parent and child transcripts as well as
	// repeated fragments in either one. Legacy rows without an id are scoped to
	// their source because no defensible cross-file identity exists for them.
	proposed := map[string]usageMessageState{}
	var order []string
	for _, sourced := range records {
		key := sourced.MessageKey
		if sourced.ProviderMessageID == "" {
			key = sourced.sourceID + "|" + key
		}
		current, ok := proposed[key]
		if !ok {
			current = session.Messages[key]
			order = append(order, key)
		}
		nextRecord := UsageRecord{
			MessageKey: key, ProviderMessageID: current.ProviderMessageID,
			Timestamp: current.Timestamp, Model: current.Model, Usage: current.Usage,
		}
		nextRecord.Usage = maxUsage(nextRecord.Usage, sourced.Usage)
		nextRecord = mergeUsageMetadata(nextRecord, sourced.UsageRecord)
		proposed[key] = usageMessageState{
			ProviderMessageID: nextRecord.ProviderMessageID,
			SourceID:          sourced.sourceID,
			Timestamp:         nextRecord.Timestamp,
			Model:             nextRecord.Model,
			Usage:             nextRecord.Usage,
		}
	}

	deltas := make([]UsageDelta, 0, len(order))
	for _, key := range order {
		next := proposed[key]
		prior, existed := session.Messages[key]
		delta := positiveUsageDelta(next.Usage, prior.Usage)
		if next != prior {
			session.Messages[key] = next
			changed = true
		}
		if !existed {
			session.MessageOrder = append(session.MessageOrder, key)
		}
		if delta.IsZero() {
			continue
		}
		deltas = append(deltas, UsageDelta{
			ProviderMessageID: next.ProviderMessageID,
			SourceID:          next.SourceID,
			Timestamp:         next.Timestamp,
			Model:             next.Model,
			Usage:             delta,
		})
	}

	if !changed {
		return nil, nil
	}
	session.LastSeen = observedAt
	boundUsageMessages(session)
	nextState := cloneUsageTrackerState(t.state)
	nextState.Sessions[sessionID] = session
	boundUsageSessions(&nextState, sessionID)
	if err := t.save(nextState); err != nil {
		return nil, err
	}
	t.state = nextState
	return deltas, nil
}

type usageSource struct {
	id   string
	path string
}

// usageSources returns a deterministic root-first inventory. filepath.WalkDir
// does not follow directory symlinks, keeping discovery inside the root
// session's own subagents directory. Only agent-*.jsonl is eligible; workflow
// journals and metadata are intentionally excluded.
func usageSources(rootTranscript string) ([]usageSource, error) {
	sources := []usageSource{{id: "root", path: rootTranscript}}
	base := subagentsDirForTranscript(rootTranscript)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) && path == base {
				return fs.SkipDir
			}
			return walkErr
		}
		if path == base || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, subagentFilePrefix) || !strings.HasSuffix(name, subagentJSONLSuffix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		sources = append(sources, usageSource{
			id:   "child:" + strings.TrimSuffix(filepath.ToSlash(rel), subagentJSONLSuffix),
			path: path,
		})
		if len(sources) > maxUsageSourcesPerSession {
			return fmt.Errorf("transcript: more than %d usage sources for one session", maxUsageSourcesPerSession)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return sources, nil
	}
	if err != nil {
		return nil, fmt.Errorf("discover Claude child transcripts: %w", err)
	}
	return sources, nil
}

// usageSourceGeneration hashes the first complete line (or a bounded partial
// prefix) to detect truncation/replacement even when the new file is not
// shorter. It stores only the digest. Append-only growth leaves the generation
// stable once the first line is complete.
func usageSourceGeneration(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	prefix, err := io.ReadAll(io.LimitReader(f, usageGenerationPrefixBytes))
	if err != nil {
		return "", 0, err
	}
	if i := strings.IndexByte(string(prefix), '\n'); i >= 0 {
		prefix = prefix[:i+1]
	}
	sum := sha256.Sum256(prefix)
	return hex.EncodeToString(sum[:]), info.Size(), nil
}

func newUsageSessionState() *usageSessionState {
	return &usageSessionState{
		Sources:  map[string]usageSourceCursor{},
		Messages: map[string]usageMessageState{},
	}
}

func normalizeUsageSessionState(session *usageSessionState) {
	if session == nil {
		return
	}
	if session.Sources == nil {
		session.Sources = map[string]usageSourceCursor{}
	}
	if session.Messages == nil {
		session.Messages = map[string]usageMessageState{}
	}
}

func cloneUsageSessionState(source *usageSessionState) *usageSessionState {
	if source == nil {
		return nil
	}
	clone := &usageSessionState{
		LastSeen:     source.LastSeen,
		Sources:      make(map[string]usageSourceCursor, len(source.Sources)),
		Messages:     make(map[string]usageMessageState, len(source.Messages)),
		MessageOrder: append([]string(nil), source.MessageOrder...),
	}
	for key, value := range source.Sources {
		clone.Sources[key] = value
	}
	for key, value := range source.Messages {
		clone.Messages[key] = value
	}
	return clone
}

func cloneUsageTrackerState(source usageTrackerState) usageTrackerState {
	clone := usageTrackerState{Version: usageCursorVersion, Sessions: make(map[string]*usageSessionState, len(source.Sessions)+1)}
	for key, value := range source.Sessions {
		clone.Sessions[key] = value
	}
	return clone
}

func boundUsageMessages(session *usageSessionState) {
	if len(session.MessageOrder) <= maxUsageMessagesPerSession {
		return
	}
	drop := len(session.MessageOrder) - maxUsageMessagesPerSession
	for _, key := range session.MessageOrder[:drop] {
		delete(session.Messages, key)
	}
	session.MessageOrder = append([]string(nil), session.MessageOrder[drop:]...)
}

func boundUsageSessions(state *usageTrackerState, keep string) {
	if len(state.Sessions) <= maxUsageSessions {
		return
	}
	type candidate struct {
		id   string
		seen time.Time
	}
	var candidates []candidate
	for id, session := range state.Sessions {
		if id != keep {
			candidates = append(candidates, candidate{id: id, seen: session.LastSeen})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].seen.Equal(candidates[j].seen) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].seen.Before(candidates[j].seen)
	})
	for _, candidate := range candidates {
		if len(state.Sessions) <= maxUsageSessions {
			break
		}
		delete(state.Sessions, candidate.id)
	}
}

func (t *UsageTracker) save(state usageTrackerState) error {
	if err := os.MkdirAll(t.stateDir, 0o755); err != nil {
		return fmt.Errorf("create Claude usage cursor directory: %w", err)
	}
	temp, err := os.CreateTemp(t.stateDir, ".claude-usage-cursors-*.tmp")
	if err != nil {
		return fmt.Errorf("create Claude usage cursor temp file: %w", err)
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("protect Claude usage cursor temp file: %w", err)
	}
	encoder := json.NewEncoder(temp)
	if err := encoder.Encode(state); err != nil {
		return fmt.Errorf("encode Claude usage cursor: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync Claude usage cursor: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close Claude usage cursor: %w", err)
	}
	closed = true
	if err := os.Rename(tempPath, t.statePath); err != nil {
		return fmt.Errorf("replace Claude usage cursor: %w", err)
	}
	return nil
}
