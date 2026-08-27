package remotestate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

var testNow = time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)

func testSnapshot(pid int) *state.Snapshot {
	snapshot := state.Snapshot{
		SchemaVersion: state.CurrentSchemaVersion,
		Sessions:      []state.Session{},
		UpdatedAt:     testNow,
	}
	if pid > 0 {
		snapshot.Sessions = append(snapshot.Sessions, state.Session{
			PID:       pid,
			CWD:       "/work",
			TTY:       "/dev/pts/1",
			StartedAt: testNow.Add(-time.Minute),
		})
	}
	return &snapshot
}

func encodedFrame(t *testing.T, host string, snapshot *state.Snapshot) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := EncodeFrame(&output, Frame{Host: host, Snapshot: snapshot}, 0); err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	return output.Bytes()
}

func TestCanonicalHostname(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "BuildBox", want: "buildbox", ok: true},
		{input: "build.example.test.", want: "build.example.test", ok: true},
		{input: "  BUILD_1  ", want: "build_1", ok: true},
		{input: "", ok: false},
		{input: "build box", ok: false},
		{input: "build..box", ok: false},
		{input: "build/box", ok: false},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := CanonicalHostname(test.input)
			if test.ok && (err != nil || got != test.want) {
				t.Fatalf("CanonicalHostname(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
			if !test.ok && err == nil {
				t.Fatalf("CanonicalHostname(%q) unexpectedly succeeded as %q", test.input, got)
			}
		})
	}
}

func TestEncodeDecodeFrameRoundTrip(t *testing.T) {
	want := Frame{Host: "buildbox", Snapshot: testSnapshot(42)}
	body := encodedFrame(t, want.Host, want.Snapshot)
	got, err := DecodeFrame(bytes.TrimSuffix(body, []byte("\n")))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDecodeFrameRejectsPartialAndIncompatibleDocuments(t *testing.T) {
	validSession := `{"pid":42,"cwd":"/work","tty":"/dev/pts/1","started_at":"2026-08-24T19:59:00Z","focused":false}`
	tests := []struct {
		name string
		body string
		is   error
	}{
		{name: "empty", body: ``, is: ErrInvalidFrame},
		{name: "noncanonical host", body: `{"host":"BuildBox","snapshot":{"schema_version":3,"sessions":[],"updated_at":"2026-08-24T20:00:00Z"}}`, is: ErrInvalidFrame},
		{name: "missing snapshot", body: `{"host":"buildbox"}`, is: ErrInvalidFrame},
		{name: "missing sessions", body: `{"host":"buildbox","snapshot":{"schema_version":3,"updated_at":"2026-08-24T20:00:00Z"}}`, is: ErrInvalidFrame},
		{name: "null sessions", body: `{"host":"buildbox","snapshot":{"schema_version":3,"sessions":null,"updated_at":"2026-08-24T20:00:00Z"}}`, is: ErrInvalidFrame},
		{name: "old schema", body: `{"host":"buildbox","snapshot":{"schema_version":2,"sessions":[],"updated_at":"2026-08-24T20:00:00Z"}}`, is: ErrSchemaMismatch},
		{name: "zero update", body: `{"host":"buildbox","snapshot":{"schema_version":3,"sessions":[],"updated_at":"0001-01-01T00:00:00Z"}}`, is: ErrInvalidFrame},
		{name: "missing pid", body: `{"host":"buildbox","snapshot":{"schema_version":3,"sessions":[{"started_at":"2026-08-24T19:59:00Z"}],"updated_at":"2026-08-24T20:00:00Z"}}`, is: ErrInvalidFrame},
		{name: "missing started at", body: `{"host":"buildbox","snapshot":{"schema_version":3,"sessions":[{"pid":42}],"updated_at":"2026-08-24T20:00:00Z"}}`, is: ErrInvalidFrame},
		{name: "duplicate pid", body: `{"host":"buildbox","snapshot":{"schema_version":3,"sessions":[` + validSession + `,` + validSession + `],"updated_at":"2026-08-24T20:00:00Z"}}`, is: ErrInvalidFrame},
		{name: "trailing object", body: `{"host":"buildbox","snapshot":{"schema_version":3,"sessions":[],"updated_at":"2026-08-24T20:00:00Z"}} {}`, is: ErrInvalidFrame},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeFrame([]byte(test.body))
			if !errors.Is(err, test.is) {
				t.Fatalf("DecodeFrame error = %v, want errors.Is(_, %v)", err, test.is)
			}
		})
	}
}

func TestDecodeFrameAllowsAdditiveEnvelopeFields(t *testing.T) {
	body := `{"host":"buildbox","snapshot":{"schema_version":3,"sessions":[],"updated_at":"2026-08-24T20:00:00Z"},"future_metadata":true}`
	if _, err := DecodeFrame([]byte(body)); err != nil {
		t.Fatalf("additive envelope field caused disconnect: %v", err)
	}
}

func TestEncodeFrameRejectsOversizeBeforeWriting(t *testing.T) {
	snapshot := testSnapshot(42)
	snapshot.Sessions[0].CWD = strings.Repeat("x", 200)
	var output bytes.Buffer
	err := EncodeFrame(&output, Frame{Host: "buildbox", Snapshot: snapshot}, 64)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("EncodeFrame error = %v, want ErrFrameTooLarge", err)
	}
	if output.Len() != 0 {
		t.Fatalf("oversized frame wrote %d-byte prefix", output.Len())
	}
}

func TestReadFramesReadsCompleteReplacementsAndRejectsOversize(t *testing.T) {
	input := append(encodedFrame(t, "buildbox", testSnapshot(1)), encodedFrame(t, "buildbox", testSnapshot(2))...)
	var pids []int
	err := ReadFrames(bytes.NewReader(input), 0, func(frame Frame) error {
		pids = append(pids, frame.Snapshot.Sessions[0].PID)
		return nil
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrames error = %v, want EOF", err)
	}
	if !reflect.DeepEqual(pids, []int{1, 2}) {
		t.Fatalf("pids = %v, want [1 2]", pids)
	}

	err = ReadFrames(strings.NewReader(strings.Repeat("x", 65)+"\n"), 64, func(Frame) error { return nil })
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize ReadFrames error = %v, want ErrFrameTooLarge", err)
	}
}

type fakeSubscriptionClient struct {
	sent      []rpc.Request
	responses []rpc.Response
	next      int
}

func (c *fakeSubscriptionClient) Send(request rpc.Request) error {
	c.sent = append(c.sent, request)
	return nil
}

func (c *fakeSubscriptionClient) Recv(response *rpc.Response) error {
	if c.next == len(c.responses) {
		return io.EOF
	}
	*response = c.responses[c.next]
	c.next++
	return nil
}

func TestStreamLocalAnnouncesBeforeSubscribingAndEmitsCanonicalFrames(t *testing.T) {
	first := testSnapshot(1)
	second := testSnapshot(2)
	client := &fakeSubscriptionClient{responses: []rpc.Response{
		{OK: true},
		{Snapshot: first},
		{Snapshot: second},
	}}
	var output bytes.Buffer
	options := StreamOptions{
		Hostname:  func() (string, error) { return "BuildBox.", nil },
		Keepalive: -1,
		OnAttach: func(context.Context) error {
			if err := client.Send(rpc.Request{Cmd: "announce-bindings"}); err != nil {
				return err
			}
			var response rpc.Response
			return client.Recv(&response)
		},
	}
	err := StreamLocal(context.Background(), client, &output, options)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("StreamLocal error = %v, want wrapped EOF", err)
	}
	if got, want := []string{client.sent[0].Cmd, client.sent[1].Cmd}, []string{"announce-bindings", "subscribe"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request order = %v, want %v", got, want)
	}
	var got []Frame
	err = ReadFrames(bytes.NewReader(output.Bytes()), 0, func(frame Frame) error {
		got = append(got, frame)
		return nil
	})
	if !errors.Is(err, io.EOF) || len(got) != 2 {
		t.Fatalf("stream output: frames=%d err=%v", len(got), err)
	}
	if got[0].Host != "buildbox" || got[0].Snapshot.Sessions[0].PID != 1 || got[1].Snapshot.Sessions[0].PID != 2 {
		t.Fatalf("unexpected frames: %#v", got)
	}
}

func TestStreamLocalStopsBeforeSubscribeWhenAttachFails(t *testing.T) {
	client := &fakeSubscriptionClient{}
	want := errors.New("announce failed")
	err := StreamLocal(context.Background(), client, io.Discard, StreamOptions{
		Hostname: func() (string, error) { return "buildbox", nil },
		OnAttach: func(context.Context) error { return want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("StreamLocal error = %v, want wrapped attach error", err)
	}
	if len(client.sent) != 0 {
		t.Fatalf("sent requests after failed attach: %+v", client.sent)
	}
}

func TestFrameJSONUsesCompleteSnapshotObject(t *testing.T) {
	body := encodedFrame(t, "buildbox", testSnapshot(0))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw["host"]) == 0 || len(raw["snapshot"]) == 0 {
		t.Fatalf("frame fields missing: %s", body)
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(raw["snapshot"], &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schema_version", "sessions", "updated_at"} {
		if _, ok := snapshot[field]; !ok {
			t.Errorf("snapshot missing %q: %s", field, body)
		}
	}
}
