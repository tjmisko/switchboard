package transcript

import (
	"bufio"
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
	"syscall"
	"time"
)

const (
	usageCursorVersion         = 2
	usageCursorFilename        = "claude-usage-cursors-v1.json"
	maxUsageSourcesPerSession  = 4096
	maxUsageMessagesPerSession = 32768
	maxUsageSessions           = 1024
	usageAnchorBytes           = 4096
)

const UsageCoveragePartialLegacy = "partial_legacy_cutover"

// UsageSnapshot is an authoritative upsert for one logical Claude provider
// message. UsageEventID stays stable across duplicate fragments, revisions,
// cursor loss, and source replacement. Consumers replace an older snapshot
// carrying the same id rather than adding the counters again.
type UsageSnapshot struct {
	UsageEventID      string
	UsageRevision     int64
	ProviderMessageID string
	SourceID          string
	Timestamp         time.Time
	Model             string
	Usage             Usage
	Cutover           bool
	Coverage          string
}

// UsageBatchAppender must durably append all supplied snapshots or return an
// error. It may partially append before failing: retry safety comes from the
// stable UsageEventID and latest-wins snapshot contract.
type UsageBatchAppender func([]UsageSnapshot) error

type usageTrackerState struct {
	Version              int                           `json:"version"`
	RevisionClock        int64                         `json:"revision_clock,omitempty"`
	LegacyCutoverAt      time.Time                     `json:"legacy_cutover_at,omitempty"`
	LegacySessions       map[string]bool               `json:"legacy_sessions,omitempty"`
	LegacyUnknownSession bool                          `json:"legacy_unknown_session,omitempty"`
	Sessions             map[string]*usageSessionState `json:"sessions"`
}

type usageSessionState struct {
	LastSeen      time.Time                    `json:"last_seen"`
	CutoverPrimed bool                         `json:"cutover_primed,omitempty"`
	UsageCoverage string                       `json:"usage_coverage,omitempty"`
	Sources       map[string]usageSourceCursor `json:"sources"`
	Messages      map[string]usageMessageState `json:"messages"`
	MessageOrder  []string                     `json:"message_order"`
}

type usageSourceCursor struct {
	Offset       int64  `json:"offset"`
	FileIdentity string `json:"file_identity,omitempty"`
	HeadBytes    int64  `json:"head_bytes,omitempty"`
	HeadAnchor   string `json:"head_anchor,omitempty"`
	TailAnchor   string `json:"tail_anchor,omitempty"`
}

type usageMessageState struct {
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	SourceID          string    `json:"source_id"`
	Timestamp         time.Time `json:"timestamp,omitempty"`
	Model             string    `json:"model,omitempty"`
	Usage             Usage     `json:"usage"`
	Revision          int64     `json:"revision"`
	LegacySuppressed  bool      `json:"legacy_suppressed,omitempty"`
}

// UsageTracker owns restart-safe Claude transcript cursors and authoritative
// message snapshots. Its state contains only hashed/opaque ids, pricing
// dimensions, counters, anchors, and timestamps; filesystem paths and content
// are never persisted.
type UsageTracker struct {
	mu           sync.Mutex
	stateDir     string
	statePath    string
	state        usageTrackerState
	messageLimit int
	// persistOverride is a synthetic failure hook used by package tests.
	persistOverride func(usageTrackerState) error
}

// NewUsageTracker loads the durable cursor. If no cursor exists, it inspects
// only safe history metadata to detect pre-upgrade Claude usage samples. Such a
// store enters explicit partial-coverage cutover mode instead of replaying a
// transcript on top of legacy additive totals.
func NewUsageTracker(stateDir string) (*UsageTracker, error) {
	return newUsageTracker(stateDir, time.Now().UTC())
}

func newUsageTracker(stateDir string, now time.Time) (*UsageTracker, error) {
	if stateDir == "" {
		return nil, errors.New("transcript: usage cursor state directory is required")
	}
	t := &UsageTracker{
		stateDir: stateDir, statePath: filepath.Join(stateDir, usageCursorFilename),
		state:        usageTrackerState{Version: usageCursorVersion, Sessions: map[string]*usageSessionState{}},
		messageLimit: maxUsageMessagesPerSession,
	}
	raw, err := os.ReadFile(t.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		legacy, err := legacyClaudeUsageMetadata(stateDir)
		if err != nil {
			return nil, err
		}
		if len(legacy.sessions) > 0 || legacy.unknownSession {
			t.state.LegacyCutoverAt = now
			t.state.LegacySessions = legacy.sessions
			t.state.LegacyUnknownSession = legacy.unknownSession
		}
		return t, nil
	}
	if err != nil {
		return nil, safeTrackerError("load cursor", err)
	}
	var state usageTrackerState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, errors.New("transcript: decode usage cursor: invalid_json")
	}
	switch state.Version {
	case usageCursorVersion:
	case 1:
		// v1 committed cursor maxima before an asynchronous history enqueue and
		// emitted additive deltas without stable ids. Re-prime each known session
		// under a visible partial cutover instead of mixing the two semantics.
		state.Version = usageCursorVersion
		state.LegacyCutoverAt = now
		state.LegacySessions = make(map[string]bool, len(state.Sessions))
		for sessionID, session := range state.Sessions {
			if session != nil {
				state.LegacySessions[sessionID] = true
				session.CutoverPrimed = false
				session.UsageCoverage = ""
				// v1 source and fallback-message keys can contain unhashed logical
				// source ids. Discard them and safely re-prime instead of carrying
				// path-shaped identifiers into the privacy-hardened state.
				session.Sources = map[string]usageSourceCursor{}
				session.Messages = map[string]usageMessageState{}
				session.MessageOrder = nil
			}
		}
	default:
		return nil, fmt.Errorf("transcript: decode usage cursor: unsupported_version_%d", state.Version)
	}
	if state.Sessions == nil {
		state.Sessions = map[string]*usageSessionState{}
	}
	if !state.LegacyCutoverAt.IsZero() && state.LegacySessions == nil && !state.LegacyUnknownSession {
		// Conservatively interpret an early v2 cursor written before legacy
		// sessions were scoped as applying to every session.
		state.LegacyUnknownSession = true
	}
	for _, session := range state.Sessions {
		normalizeUsageSessionState(session)
	}
	t.state = state
	return t, nil
}

// SyncSession discovers, parses, and durably appends one session's changed
// snapshots before atomically committing its cursor. An append failure leaves
// the cursor untouched. A cursor-save failure after a successful append retries
// the same stable ids next time, which latest-wins consumers collapse safely.
func (t *UsageTracker) SyncSession(sessionID, rootTranscript string, observedAt time.Time, appendBatch UsageBatchAppender) ([]UsageSnapshot, error) {
	if sessionID == "" || rootTranscript == "" {
		return nil, errors.New("transcript: session id and root transcript are required")
	}
	if appendBatch == nil {
		return nil, errors.New("transcript: durable usage appender is required")
	}
	sources, err := usageSources(rootTranscript)
	if err != nil {
		return nil, safeTrackerError("discover child sources", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	revision := nextUsageRevision(t.state.RevisionClock, observedAt)

	priorSession := t.state.Sessions[sessionID]
	session := cloneUsageSessionState(priorSession)
	if session == nil {
		session = newUsageSessionState()
	}
	changed := priorSession == nil
	var records []sourcedUsageRecord

	for _, source := range sources {
		sourceID := opaqueUsageSourceID(sessionID, source.id)
		cursor, existed := session.Sources[sourceID]
		valid, err := validateUsageSourceCursor(source.path, cursor)
		if err != nil {
			return nil, safeSourceError("validate", sourceID, err)
		}
		if !existed || !valid {
			cursor = usageSourceCursor{}
			changed = true
		}
		parsed, newOffset, err := UsageRecordsSince(source.path, cursor.Offset)
		if err != nil {
			return nil, safeSourceError("read", sourceID, err)
		}
		nextCursor, err := buildUsageSourceCursor(source.path, newOffset)
		if err != nil {
			return nil, safeSourceError("anchor", sourceID, err)
		}
		if nextCursor != cursor {
			changed = true
		}
		session.Sources[sourceID] = nextCursor
		for _, record := range parsed {
			records = append(records, sourcedUsageRecord{UsageRecord: record, sourceID: sourceID})
		}
	}

	needsCutover := !session.CutoverPrimed && t.needsLegacyCutover(sessionID)
	if needsCutover {
		session.CutoverPrimed = true
		session.UsageCoverage = UsageCoveragePartialLegacy
		changed = true
	}
	if !session.CutoverPrimed {
		session.CutoverPrimed = true
		changed = true
	}

	suppressBefore := time.Time{}
	if session.UsageCoverage == UsageCoveragePartialLegacy {
		suppressBefore = t.state.LegacyCutoverAt
	}
	snapshots, messagesChanged := applyUsageRecords(sessionID, session, records, suppressBefore, revision)
	changed = changed || messagesChanged
	if needsCutover {
		marker := UsageSnapshot{
			UsageEventID: stableUsageEventID(sessionID, "legacy-cutover"), UsageRevision: revision,
			Timestamp: observedAt, Cutover: true, Coverage: UsageCoveragePartialLegacy,
		}
		snapshots = append([]UsageSnapshot{marker}, snapshots...)
	}
	if !changed {
		return nil, nil
	}
	boundUsageMessages(session, t.messageLimit)
	if len(snapshots) > 0 {
		if err := appendBatch(snapshots); err != nil {
			return nil, safeTrackerError("append usage batch", err)
		}
	}
	nextState := t.nextState(sessionID, session, observedAt, revision)
	if err := t.persist(nextState); err != nil {
		return nil, err
	}
	t.state = nextState
	return snapshots, nil
}

type sourcedUsageRecord struct {
	UsageRecord
	sourceID string
}

func (t *UsageTracker) nextState(sessionID string, session *usageSessionState, observedAt time.Time, revision int64) usageTrackerState {
	session.LastSeen = observedAt
	next := cloneUsageTrackerState(t.state)
	next.Version = usageCursorVersion
	if revision > next.RevisionClock {
		next.RevisionClock = revision
	}
	next.Sessions[sessionID] = session
	boundUsageSessions(&next, sessionID)
	return next
}

// nextUsageRevision is a restart-safe, tracker-global Lamport clock seeded by
// observation time. Per-message counters can reset when bounded state evicts a
// message; a global monotonic revision guarantees that a later replay of the
// same stable UsageEventID still supersedes its older history snapshot.
func nextUsageRevision(prior int64, observedAt time.Time) int64 {
	next := observedAt.UTC().UnixNano()
	if next <= 0 {
		next = 1
	}
	if next <= prior {
		next = prior + 1
	}
	return next
}

func (t *UsageTracker) needsLegacyCutover(sessionID string) bool {
	if t.state.LegacyCutoverAt.IsZero() {
		return false
	}
	return t.state.LegacyUnknownSession || t.state.LegacySessions[sessionID]
}

func applyUsageRecords(sessionID string, session *usageSessionState, records []sourcedUsageRecord, suppressBefore time.Time, revision int64) ([]UsageSnapshot, bool) {
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
		sourceID := current.SourceID
		if sourceID == "" {
			sourceID = sourced.sourceID
		}
		proposed[key] = usageMessageState{
			ProviderMessageID: nextRecord.ProviderMessageID, SourceID: sourceID,
			Timestamp: nextRecord.Timestamp, Model: nextRecord.Model, Usage: nextRecord.Usage,
			Revision: current.Revision,
			LegacySuppressed: current.LegacySuppressed || (!suppressBefore.IsZero() &&
				(sourced.Timestamp.IsZero() || !sourced.Timestamp.After(suppressBefore))),
		}
	}

	changed := false
	snapshots := make([]UsageSnapshot, 0, len(order))
	for _, key := range order {
		next := proposed[key]
		prior, existed := session.Messages[key]
		substantive := !existed || usageMessageChanged(prior, next)
		if substantive {
			next.Revision = revision
			session.Messages[key] = next
			changed = true
		}
		if !existed {
			session.MessageOrder = append(session.MessageOrder, key)
		}
		if !substantive || next.LegacySuppressed || next.Usage.IsZero() {
			continue
		}
		snapshots = append(snapshots, UsageSnapshot{
			UsageEventID: stableUsageEventID(sessionID, key), UsageRevision: next.Revision,
			ProviderMessageID: next.ProviderMessageID, SourceID: next.SourceID,
			Timestamp: next.Timestamp, Model: next.Model, Usage: next.Usage,
			Coverage: session.UsageCoverage,
		})
	}
	return snapshots, changed
}

func usageMessageChanged(a, b usageMessageState) bool {
	return a.ProviderMessageID != b.ProviderMessageID || a.SourceID != b.SourceID ||
		!a.Timestamp.Equal(b.Timestamp) || a.Model != b.Model || a.Usage != b.Usage ||
		a.LegacySuppressed != b.LegacySuppressed
}

type usageSource struct {
	id   string
	path string
}

// usageSources returns a deterministic root-first inventory. Its logical ids
// are hashed before leaving this function's caller and are never persisted or
// logged. WalkDir does not follow directory symlinks.
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
		sources = append(sources, usageSource{id: filepath.ToSlash(rel), path: path})
		if len(sources) > maxUsageSourcesPerSession {
			return errors.New("too_many_sources")
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return sources, nil
	}
	if err != nil {
		return nil, err
	}
	return sources, nil
}

func opaqueUsageSourceID(sessionID, logicalID string) string {
	return "cusrc_" + stableHash("claude-usage-source-v1", sessionID, logicalID)[:32]
}

func stableUsageEventID(sessionID, messageKey string) string {
	return "cuev_" + stableHash("claude-usage-event-v1", sessionID, messageKey)
}

func stableHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// validateUsageSourceCursor checks an opaque file identity plus head and tail
// anchors around the consumed prefix. This catches atomic replacement and
// same-prefix in-place rewrites that size/head-only cursors miss.
func validateUsageSourceCursor(path string, cursor usageSourceCursor) (bool, error) {
	if cursor == (usageSourceCursor{}) {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if cursor.Offset < 0 || cursor.Offset > info.Size() {
		return false, nil
	}
	current, err := buildUsageSourceCursor(path, cursor.Offset)
	if err != nil {
		return false, err
	}
	return current == cursor, nil
}

func buildUsageSourceCursor(path string, offset int64) (usageSourceCursor, error) {
	f, err := os.Open(path)
	if err != nil {
		return usageSourceCursor{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return usageSourceCursor{}, err
	}
	if offset < 0 || offset > info.Size() {
		return usageSourceCursor{}, errors.New("invalid_offset")
	}
	headBytes := offset
	if headBytes > usageAnchorBytes {
		headBytes = usageAnchorBytes
	}
	head, err := readUsageAnchor(f, 0, headBytes)
	if err != nil {
		return usageSourceCursor{}, err
	}
	tailStart := offset - usageAnchorBytes
	if tailStart < 0 {
		tailStart = 0
	}
	tail, err := readUsageAnchor(f, tailStart, offset-tailStart)
	if err != nil {
		return usageSourceCursor{}, err
	}
	return usageSourceCursor{
		Offset: offset, FileIdentity: usageFileIdentity(info), HeadBytes: headBytes,
		HeadAnchor: hashBytes(head), TailAnchor: hashBytes(tail),
	}, nil
}

func readUsageAnchor(f *os.File, offset, length int64) ([]byte, error) {
	if length == 0 {
		return nil, nil
	}
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(n) != length {
		return nil, io.ErrUnexpectedEOF
	}
	return buf, nil
}

func usageFileIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return stableHash("claude-usage-file-v1", fmt.Sprintf("%d", stat.Dev), fmt.Sprintf("%d", stat.Ino))[:32]
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type legacyUsageMetadata struct {
	sessions       map[string]bool
	unknownSession bool
}

func legacyClaudeUsageMetadata(dir string) (legacyUsageMetadata, error) {
	metadata := legacyUsageMetadata{sessions: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return metadata, nil
	}
	if err != nil {
		return metadata, safeTrackerError("scan legacy history", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if _, err := time.Parse("2006-01-02", strings.TrimSuffix(name, ".jsonl")); err != nil {
			continue
		}
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return metadata, safeTrackerError("scan legacy history", err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var event struct {
				Type         string `json:"type"`
				Agent        string `json:"agent"`
				SessionID    string `json:"session_id"`
				UsageEventID string `json:"usage_event_id"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "usage_sample" && event.Agent == "claude" && event.UsageEventID == "" {
				if event.SessionID == "" {
					metadata.unknownSession = true
				} else {
					metadata.sessions[event.SessionID] = true
				}
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return metadata, safeTrackerError("scan legacy history", scanErr)
		}
		if closeErr != nil {
			return metadata, safeTrackerError("scan legacy history", closeErr)
		}
	}
	return metadata, nil
}

func safeSourceError(operation, sourceID string, err error) error {
	return fmt.Errorf("transcript: %s usage source %s: %s", operation, sourceID, safeErrorKind(err))
}

func safeTrackerError(operation string, err error) error {
	return fmt.Errorf("transcript: %s: %s", operation, safeErrorKind(err))
}

func safeErrorKind(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "changed_during_read"
	default:
		return "io_error"
	}
}

func newUsageSessionState() *usageSessionState {
	return &usageSessionState{Sources: map[string]usageSourceCursor{}, Messages: map[string]usageMessageState{}}
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
	seen := map[string]bool{}
	order := make([]string, 0, len(session.Messages))
	for _, key := range session.MessageOrder {
		if _, ok := session.Messages[key]; ok && !seen[key] {
			seen[key] = true
			order = append(order, key)
		}
	}
	var missing []string
	for key := range session.Messages {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	session.MessageOrder = append(order, missing...)
}

func cloneUsageSessionState(source *usageSessionState) *usageSessionState {
	if source == nil {
		return nil
	}
	clone := &usageSessionState{
		LastSeen: source.LastSeen, CutoverPrimed: source.CutoverPrimed, UsageCoverage: source.UsageCoverage,
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
	clone := usageTrackerState{
		Version: usageCursorVersion, RevisionClock: source.RevisionClock, LegacyCutoverAt: source.LegacyCutoverAt,
		LegacyUnknownSession: source.LegacyUnknownSession,
		LegacySessions:       make(map[string]bool, len(source.LegacySessions)),
		Sessions:             make(map[string]*usageSessionState, len(source.Sessions)+1),
	}
	for key, value := range source.LegacySessions {
		clone.LegacySessions[key] = value
	}
	for key, value := range source.Sessions {
		clone.Sessions[key] = value
	}
	return clone
}

func boundUsageMessages(session *usageSessionState, limit int) {
	if limit <= 0 || len(session.MessageOrder) <= limit {
		return
	}
	drop := len(session.MessageOrder) - limit
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

func (t *UsageTracker) persist(state usageTrackerState) error {
	if t.persistOverride != nil {
		if err := t.persistOverride(state); err != nil {
			return safeTrackerError("persist cursor", err)
		}
		return nil
	}
	return t.save(state)
}

func (t *UsageTracker) save(state usageTrackerState) error {
	if err := os.MkdirAll(t.stateDir, 0o755); err != nil {
		return safeTrackerError("persist cursor", err)
	}
	temp, err := os.CreateTemp(t.stateDir, ".claude-usage-cursors-*.tmp")
	if err != nil {
		return safeTrackerError("persist cursor", err)
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
		return safeTrackerError("persist cursor", err)
	}
	if err := json.NewEncoder(temp).Encode(state); err != nil {
		return safeTrackerError("persist cursor", err)
	}
	if err := temp.Sync(); err != nil {
		return safeTrackerError("persist cursor", err)
	}
	if err := temp.Close(); err != nil {
		return safeTrackerError("persist cursor", err)
	}
	closed = true
	if err := os.Rename(tempPath, t.statePath); err != nil {
		return safeTrackerError("persist cursor", err)
	}
	dir, err := os.Open(t.stateDir)
	if err != nil {
		return safeTrackerError("persist cursor", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return safeTrackerError("persist cursor", err)
	}
	if err := dir.Close(); err != nil {
		return safeTrackerError("persist cursor", err)
	}
	return nil
}
