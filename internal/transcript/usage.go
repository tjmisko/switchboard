package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// usageWire accepts both Claude's nested transcript representation and the
// flat representation used by the durable usage cursor. Keeping this decoder
// here makes the persisted state forward-compatible with the source parser
// without teaching callers about Claude's wire-only nesting.
type usageWire struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`

	CacheWrite5mTokens  int64 `json:"cache_write_5m_input_tokens"`
	CacheWrite1hTokens  int64 `json:"cache_write_1h_input_tokens"`
	CacheCreationDetail bool  `json:"cache_creation_detail"`
	CacheCreation       *struct {
		Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`

	ServiceTier  string `json:"service_tier"`
	Speed        string `json:"speed"`
	InferenceGeo string `json:"inference_geo"`

	WebSearchRequests int64 `json:"web_search_requests"`
	WebFetchRequests  int64 `json:"web_fetch_requests"`
	ServerToolUse     struct {
		WebSearchRequests int64 `json:"web_search_requests"`
		WebFetchRequests  int64 `json:"web_fetch_requests"`
	} `json:"server_tool_use"`
}

// UnmarshalJSON preserves Claude's nested cache-TTL and server-tool fields.
// Claude also emits cache_creation_input_tokens; when the detailed TTL fields
// are present, their sum is the legacy combined value by definition and is kept
// as such for older history consumers.
func (u *Usage) UnmarshalJSON(data []byte) error {
	var wire usageWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	u.InputTokens = wire.InputTokens
	u.OutputTokens = wire.OutputTokens
	u.CacheReadTokens = wire.CacheReadTokens
	u.CacheCreationTokens = wire.CacheCreationTokens
	u.CacheWrite5mTokens = wire.CacheWrite5mTokens
	u.CacheWrite1hTokens = wire.CacheWrite1hTokens
	u.CacheCreationDetail = wire.CacheCreationDetail
	if wire.CacheCreation != nil {
		u.CacheCreationDetail = true
		u.CacheWrite5mTokens = wire.CacheCreation.Ephemeral5mInputTokens
		u.CacheWrite1hTokens = wire.CacheCreation.Ephemeral1hInputTokens
		u.CacheCreationTokens = u.CacheWrite5mTokens + u.CacheWrite1hTokens
	}
	u.ServiceTier = wire.ServiceTier
	u.Speed = wire.Speed
	u.InferenceGeo = wire.InferenceGeo
	u.WebSearchRequests = wire.WebSearchRequests
	u.WebFetchRequests = wire.WebFetchRequests
	if wire.ServerToolUse.WebSearchRequests != 0 || wire.ServerToolUse.WebFetchRequests != 0 {
		u.WebSearchRequests = wire.ServerToolUse.WebSearchRequests
		u.WebFetchRequests = wire.ServerToolUse.WebFetchRequests
	}
	return nil
}

// UsageRecord is one complete assistant usage observation parsed from a
// transcript. MessageKey is an internal, content-free dedup key: the provider
// message id when present, otherwise the transcript row UUID, otherwise a hash
// of the synthetic/legacy row. ProviderMessageID is left empty for fallbacks so
// downstream consumers never mistake a local key for a provider identity.
type UsageRecord struct {
	MessageKey        string
	ProviderMessageID string
	Timestamp         time.Time
	Model             string
	Usage             Usage
}

// UsageRecordsSince decodes complete assistant usage rows appended since
// byteOffset. It deliberately returns every fragment; collapseUsageRecords and
// UsageTracker resolve repeated/revised provider message ids without losing the
// final authoritative counters.
func UsageRecordsSince(path string, byteOffset int64) ([]UsageRecord, int64, error) {
	complete, newOffset, err := readNewLines(path, byteOffset)
	if err != nil || len(complete) == 0 {
		return nil, newOffset, err
	}
	var records []UsageRecord
	for _, raw := range bytes.Split(complete, []byte{'\n'}) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var e entry
		if json.Unmarshal(raw, &e) != nil || e.Message.Role != "assistant" || e.Message.Usage == nil {
			continue
		}
		key := ""
		switch {
		case e.Message.ID != "":
			key = "message:" + e.Message.ID
		case e.UUID != "":
			key = "entry:" + e.UUID
		default:
			sum := sha256.Sum256(raw)
			key = "sha256:" + hex.EncodeToString(sum[:])
		}
		ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		records = append(records, UsageRecord{
			MessageKey: key, ProviderMessageID: e.Message.ID, Timestamp: ts,
			Model: e.Message.Model, Usage: *e.Message.Usage,
		})
	}
	return records, newOffset, nil
}

// collapseUsageRecords makes the stateless compatibility readers correct for
// repeated fragments contained in one read. The stateful UsageTracker applies
// the same monotonic merge across polling and daemon-restart boundaries.
func collapseUsageRecords(records []UsageRecord) []UsageRecord {
	byKey := make(map[string]UsageRecord, len(records))
	order := make([]string, 0, len(records))
	for _, record := range records {
		prior, ok := byKey[record.MessageKey]
		if !ok {
			byKey[record.MessageKey] = record
			order = append(order, record.MessageKey)
			continue
		}
		prior.Usage = maxUsage(prior.Usage, record.Usage)
		prior = mergeUsageMetadata(prior, record)
		byKey[record.MessageKey] = prior
	}
	out := make([]UsageRecord, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

func mergeUsageMetadata(prior, next UsageRecord) UsageRecord {
	if next.ProviderMessageID != "" {
		prior.ProviderMessageID = next.ProviderMessageID
	}
	if next.Model != "" {
		prior.Model = next.Model
	}
	if next.Usage.ServiceTier != "" {
		prior.Usage.ServiceTier = next.Usage.ServiceTier
	}
	if next.Usage.Speed != "" {
		prior.Usage.Speed = next.Usage.Speed
	}
	if next.Usage.InferenceGeo != "" {
		prior.Usage.InferenceGeo = next.Usage.InferenceGeo
	}
	if !next.Timestamp.IsZero() && (prior.Timestamp.IsZero() || next.Timestamp.Before(prior.Timestamp)) {
		prior.Timestamp = next.Timestamp
	}
	return prior
}

func maxUsage(a, b Usage) Usage {
	if b.InputTokens > a.InputTokens {
		a.InputTokens = b.InputTokens
	}
	if b.OutputTokens > a.OutputTokens {
		a.OutputTokens = b.OutputTokens
	}
	if b.CacheReadTokens > a.CacheReadTokens {
		a.CacheReadTokens = b.CacheReadTokens
	}
	if b.CacheCreationDetail {
		a.CacheCreationDetail = true
		a.CacheWrite5mTokens = b.CacheWrite5mTokens
		a.CacheWrite1hTokens = b.CacheWrite1hTokens
		a.CacheCreationTokens = b.CacheWrite5mTokens + b.CacheWrite1hTokens
	} else if !a.CacheCreationDetail && b.CacheCreationTokens > a.CacheCreationTokens {
		a.CacheCreationTokens = b.CacheCreationTokens
	}
	if b.WebSearchRequests > a.WebSearchRequests {
		a.WebSearchRequests = b.WebSearchRequests
	}
	if b.WebFetchRequests > a.WebFetchRequests {
		a.WebFetchRequests = b.WebFetchRequests
	}
	if b.ServiceTier != "" {
		a.ServiceTier = b.ServiceTier
	}
	if b.Speed != "" {
		a.Speed = b.Speed
	}
	if b.InferenceGeo != "" {
		a.InferenceGeo = b.InferenceGeo
	}
	return a
}
