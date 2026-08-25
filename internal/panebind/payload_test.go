package panebind

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func exact(host string, pid int, started string) ExactSessionKey {
	t, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		panic(err)
	}
	return ExactSessionKey{Hostname: host, PID: pid, StartedAt: t}
}

func TestPayloadRoundTripIsVersionedCanonicalAndBounded(t *testing.T) {
	want := exact("buildbox", 1234, "2026-08-24T20:00:00.123456789-07:00")
	payload, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(payload) > MaxPayloadBytes {
		t.Fatalf("payload length = %d, max %d", len(payload), MaxPayloadBytes)
	}
	if !strings.HasPrefix(payload, `{"v":1,"host":"buildbox","pid":1234,"started_at":`) {
		t.Fatalf("unexpected payload: %s", payload)
	}
	got, err := Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if got.StartedAt.Location() != time.UTC {
		t.Fatalf("decoded location = %v, want UTC", got.StartedAt.Location())
	}
}

func TestDecodeRejectsUnboundedOrNonV1Payloads(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr error
	}{
		{"empty", "", ErrMalformedPayload},
		{"too large", strings.Repeat("x", MaxPayloadBytes+1), ErrPayloadTooLarge},
		{"malformed", `{`, ErrMalformedPayload},
		{"missing version", `{"host":"h","pid":1,"started_at":"2026-08-24T20:00:00Z"}`, ErrPayloadVersion},
		{"future version", `{"v":2,"host":"h","pid":1,"started_at":"2026-08-24T20:00:00Z"}`, ErrPayloadVersion},
		{"unknown field", `{"v":1,"host":"h","pid":1,"started_at":"2026-08-24T20:00:00Z","command":"oops"}`, ErrMalformedPayload},
		{"trailing document", `{"v":1,"host":"h","pid":1,"started_at":"2026-08-24T20:00:00Z"}{}`, ErrMalformedPayload},
		{"zero pid", `{"v":1,"host":"h","pid":0,"started_at":"2026-08-24T20:00:00Z"}`, ErrInvalidSession},
		{"zero time", `{"v":1,"host":"h","pid":1,"started_at":"0001-01-01T00:00:00Z"}`, ErrInvalidSession},
		{"control hostname", `{"v":1,"host":"bad\nhost","pid":1,"started_at":"2026-08-24T20:00:00Z"}`, ErrInvalidSession},
		{"uppercase hostname", `{"v":1,"host":"BuildBox","pid":1,"started_at":"2026-08-24T20:00:00Z"}`, ErrInvalidSession},
		{"trailing-dot hostname", `{"v":1,"host":"buildbox.","pid":1,"started_at":"2026-08-24T20:00:00Z"}`, ErrInvalidSession},
		{"space hostname", `{"v":1,"host":"build box","pid":1,"started_at":"2026-08-24T20:00:00Z"}`, ErrInvalidSession},
		{"empty label", `{"v":1,"host":"build..box","pid":1,"started_at":"2026-08-24T20:00:00Z"}`, ErrInvalidSession},
		{"slash hostname", `{"v":1,"host":"build/box","pid":1,"started_at":"2026-08-24T20:00:00Z"}`, ErrInvalidSession},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(tt.payload)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Decode error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestEncodeRejectsInvalidKey(t *testing.T) {
	tests := []ExactSessionKey{
		{},
		{Hostname: " host", PID: 1, StartedAt: time.Now()},
		{Hostname: "host", PID: -1, StartedAt: time.Now()},
		{Hostname: strings.Repeat("h", MaxHostnameBytes+1), PID: 1, StartedAt: time.Now()},
	}
	for _, key := range tests {
		if _, err := Encode(key); !errors.Is(err, ErrInvalidSession) {
			t.Errorf("Encode(%+v) error = %v, want ErrInvalidSession", key, err)
		}
	}
}

func TestCanonicalStripsMonotonicClockAndNormalizesZone(t *testing.T) {
	started := time.Now()
	key := ExactSessionKey{Hostname: "h", PID: 1, StartedAt: started}
	roundTrip, err := Decode(mustEncode(t, key))
	if err != nil {
		t.Fatal(err)
	}
	if key.Canonical() != roundTrip {
		t.Fatalf("canonical key = %+v, decoded = %+v", key.Canonical(), roundTrip)
	}
}

func mustEncode(t *testing.T, key ExactSessionKey) string {
	t.Helper()
	payload, err := Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
