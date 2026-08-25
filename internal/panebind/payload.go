package panebind

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	PayloadVersion  = 1
	MaxPayloadBytes = 512
	VariableName    = "SWITCHBOARD_SESSION"
)

var (
	ErrPayloadTooLarge  = errors.New("panebind: payload exceeds limit")
	ErrPayloadVersion   = errors.New("panebind: unsupported payload version")
	ErrMalformedPayload = errors.New("panebind: malformed payload")
)

type wirePayload struct {
	Version   int      `json:"v"`
	Hostname  string   `json:"host"`
	PID       int      `json:"pid"`
	StartedAt jsonTime `json:"started_at"`
}

// jsonTime is an alias so the wire struct stays private without changing
// time.Time's standard RFC3339Nano JSON representation.
type jsonTime = time.Time

// Encode returns the canonical v1 JSON that WezTerm exposes as the decoded
// user-variable value. It is safe to pass as one argv element; no shell is
// involved.
func Encode(key ExactSessionKey) (string, error) {
	if err := key.Validate(); err != nil {
		return "", err
	}
	key = key.Canonical()
	b, err := json.Marshal(wirePayload{
		Version: PayloadVersion, Hostname: key.Hostname, PID: key.PID, StartedAt: key.StartedAt,
	})
	if err != nil {
		return "", fmt.Errorf("panebind: encode payload: %w", err)
	}
	if len(b) > MaxPayloadBytes {
		return "", ErrPayloadTooLarge
	}
	return string(b), nil
}

// Decode strictly parses one bounded v1 payload. Unknown fields and trailing
// JSON are rejected so future versions cannot be accidentally interpreted as
// v1.
func Decode(payload string) (ExactSessionKey, error) {
	if len(payload) == 0 {
		return ExactSessionKey{}, ErrMalformedPayload
	}
	if len(payload) > MaxPayloadBytes {
		return ExactSessionKey{}, ErrPayloadTooLarge
	}
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	var p wirePayload
	if err := dec.Decode(&p); err != nil {
		return ExactSessionKey{}, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return ExactSessionKey{}, ErrMalformedPayload
		}
		return ExactSessionKey{}, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	if p.Version != PayloadVersion {
		return ExactSessionKey{}, ErrPayloadVersion
	}
	key := ExactSessionKey{Hostname: p.Hostname, PID: p.PID, StartedAt: p.StartedAt}.Canonical()
	if err := key.Validate(); err != nil {
		return ExactSessionKey{}, err
	}
	return key, nil
}
