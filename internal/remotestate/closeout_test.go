package remotestate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestCloseoutFrameRoundTripsAndExcludesASnapshot(t *testing.T) {
	var output bytes.Buffer
	frame := Frame{Host: "buildbox", Closeout: &Closeout{Reason: CloseoutSignal}}
	if err := EncodeFrame(&output, frame, 0); err != nil {
		t.Fatalf("EncodeFrame(closeout): %v", err)
	}
	got, err := DecodeFrame(bytes.TrimSuffix(output.Bytes(), []byte("\n")))
	if err != nil {
		t.Fatalf("DecodeFrame(closeout): %v", err)
	}
	if got.Snapshot != nil || got.Closeout == nil || got.Closeout.Reason != CloseoutSignal {
		t.Fatalf("decoded closeout = %+v", got)
	}

	both := Frame{Host: "buildbox", Snapshot: testSnapshot(1), Closeout: &Closeout{Reason: CloseoutSignal}}
	if err := EncodeFrame(io.Discard, both, 0); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("encoding snapshot+closeout error = %v, want ErrInvalidFrame", err)
	}
	if _, err := DecodeFrame([]byte(snapshotJSON + `,"closeout":{"reason":"signal"}}`)); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("decoding snapshot+closeout error = %v, want ErrInvalidFrame", err)
	}
}

func TestDecodeFrameRejectsPeerTextThatCouldForgeALogRecord(t *testing.T) {
	// The reason reaches a log line, so it is constrained rather than trusted.
	for _, reason := range []string{
		"host=evil category=connected",
		"signal\nremote-state: destination=x",
		"SIGNAL",
		strings.Repeat("x", maxCloseoutReasonBytes+1),
	} {
		body := []byte(`{"host":"buildbox","closeout":{"reason":` + quoteJSON(reason) + `}}`)
		if _, err := DecodeFrame(body); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("DecodeFrame(reason=%q) error = %v, want ErrInvalidFrame", reason, err)
		}
	}
	// An empty reason and an unrecognized-but-well-formed token are both
	// accepted: the drop decision must not depend on having heard of the reason.
	for _, body := range []string{
		`{"host":"buildbox","closeout":{}}`,
		`{"host":"buildbox","closeout":{"reason":"some-future-reason"}}`,
	} {
		frame, err := DecodeFrame([]byte(body))
		if err != nil || frame.Closeout == nil {
			t.Fatalf("DecodeFrame(%s) = %+v, %v", body, frame, err)
		}
	}
}

// snapshotJSON is the opening of a wire frame carrying a minimal valid snapshot
// at the CURRENT schema version, so these tests exercise field handling rather
// than re-failing the version check every time the schema moves.
var snapshotJSON = `{"host":"buildbox","snapshot":{"schema_version":` +
	strconv.Itoa(state.CurrentSchemaVersion) + `,"sessions":[],"updated_at":"2026-08-24T20:00:00Z"}`

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestDecodeFrameCarriesAKeepaliveAdvertisementAndBoundsIt(t *testing.T) {
	body := []byte(snapshotJSON + `,"keepalive_seconds":10}`)
	frame, err := DecodeFrame(body)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if frame.KeepaliveSeconds != 10 {
		t.Fatalf("keepalive = %d, want 10", frame.KeepaliveSeconds)
	}
	for _, seconds := range []string{"-1", "301"} {
		body := []byte(snapshotJSON + `,"keepalive_seconds":` + seconds + `}`)
		if _, err := DecodeFrame(body); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("DecodeFrame(keepalive=%s) error = %v, want ErrInvalidFrame", seconds, err)
		}
	}
}

func TestDecodeFrameIgnoresUnknownFieldsSoNewerPeersStayReadable(t *testing.T) {
	// This is the forward-compatibility guarantee an OLDER client relies on: a
	// snapshot frame from a newer remote must still decode. The transport is
	// one-way (ssh -n), so there is no handshake to fall back on.
	body := []byte(snapshotJSON + `,"some_future_field":{"a":1}}`)
	if _, err := DecodeFrame(body); err != nil {
		t.Fatalf("DecodeFrame with an unknown field: %v", err)
	}
}

func TestReadFramesReportsATruncatedTailAsTransportFailure(t *testing.T) {
	complete := encodedFrame(t, "buildbox", testSnapshot(1))
	partial := encodedFrame(t, "buildbox", testSnapshot(2))
	input := append(append([]byte{}, complete...), partial[:len(partial)/2]...)

	var got []Frame
	err := ReadFrames(bytes.NewReader(input), 0, func(frame Frame) error {
		got = append(got, frame)
		return nil
	})
	if !errors.Is(err, ErrTruncatedFrame) {
		t.Fatalf("ReadFrames error = %v, want ErrTruncatedFrame", err)
	}
	if len(got) != 1 || got[0].Snapshot.Sessions[0].PID != 1 {
		t.Fatalf("frames before the cut = %+v, want just pid 1", got)
	}
	// A cut link and a peer speaking an unreadable protocol must be answered
	// differently, so they must not collapse to the same error.
	if errors.Is(ErrTruncatedFrame, ErrInvalidFrame) {
		t.Fatal("ErrTruncatedFrame matches ErrInvalidFrame; the two are indistinguishable")
	}
}

// blockingClient hands out queued snapshots and then blocks, modelling a daemon
// with nothing to report.
type blockingClient struct {
	sent      []rpc.Request
	responses []rpc.Response
	next      int
	idle      chan struct{}
}

func (c *blockingClient) Send(request rpc.Request) error {
	c.sent = append(c.sent, request)
	return nil
}

func (c *blockingClient) Recv(response *rpc.Response) error {
	if c.next >= len(c.responses) {
		<-c.idle
		return io.EOF
	}
	*response = c.responses[c.next]
	c.next++
	return nil
}

func TestStreamLocalResendsItsCurrentSnapshotWhileTheDaemonIsQuiet(t *testing.T) {
	snapshot := testSnapshot(42)
	client := &blockingClient{
		responses: []rpc.Response{{Snapshot: snapshot}},
		idle:      make(chan struct{}),
	}
	beats := make(chan time.Time)
	var output syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- StreamLocal(context.Background(), client, &output, StreamOptions{
			Hostname:  func() (string, error) { return "buildbox", nil },
			Keepalive: 10 * time.Second,
			Ticker:    func(time.Duration) (<-chan time.Time, func()) { return beats, func() {} },
		})
	}()

	waitForFrames(t, &output, 1)
	beats <- time.Time{}
	beats <- time.Time{}
	waitForFrames(t, &output, 3)
	close(client.idle)
	if err := <-done; !errors.Is(err, io.EOF) {
		t.Fatalf("StreamLocal error = %v, want wrapped EOF", err)
	}

	frames := decodeAll(t, output.Bytes())
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 1 snapshot + 2 keepalives", len(frames))
	}
	for i, frame := range frames {
		if frame.Snapshot == nil || frame.Snapshot.Sessions[0].PID != 42 {
			t.Fatalf("frame %d = %+v, want the current snapshot re-sent verbatim", i, frame)
		}
		if frame.KeepaliveSeconds != 10 {
			t.Fatalf("frame %d advertised keepalive %d, want 10", i, frame.KeepaliveSeconds)
		}
	}
}

func TestStreamLocalWritesAFinalCloseoutAndExitsCleanlyOnTeardown(t *testing.T) {
	client := &blockingClient{
		responses: []rpc.Response{{Snapshot: testSnapshot(42)}},
		idle:      make(chan struct{}),
	}
	defer close(client.idle)
	closeout := make(chan string, 1)
	var output syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- StreamLocal(context.Background(), client, &output, StreamOptions{
			Hostname:  func() (string, error) { return "buildbox", nil },
			Keepalive: -1,
			Closeout:  closeout,
		})
	}()

	waitForFrames(t, &output, 1)
	closeout <- CloseoutSignal
	if err := <-done; err != nil {
		t.Fatalf("a teardown the operator asked for reported an error: %v", err)
	}
	frames := decodeAll(t, output.Bytes())
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want the snapshot plus one closeout", len(frames))
	}
	last := frames[len(frames)-1]
	if last.Closeout == nil || last.Closeout.Reason != CloseoutSignal || last.Snapshot != nil {
		t.Fatalf("final frame = %+v, want a bare closeout", last)
	}
}

func TestStreamLocalRejectsAKeepaliveItCouldNotHonestlyAdvertise(t *testing.T) {
	for _, period := range []time.Duration{time.Millisecond, (MaxKeepaliveSeconds + 1) * time.Second} {
		err := StreamLocal(context.Background(), &blockingClient{}, io.Discard, StreamOptions{
			Hostname:  func() (string, error) { return "buildbox", nil },
			Keepalive: period,
		})
		if err == nil {
			t.Fatalf("StreamLocal accepted keepalive %s", period)
		}
	}
}

func decodeAll(t *testing.T, body []byte) []Frame {
	t.Helper()
	var frames []Frame
	err := ReadFrames(bytes.NewReader(body), 0, func(frame Frame) error {
		frames = append(frames, frame)
		return nil
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrames: %v", err)
	}
	return frames
}

func waitForFrames(t *testing.T, output *syncBuffer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Count(output.Bytes(), []byte("\n")) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d frames; got %q", want, output.Bytes())
}

func TestObservablyEqualIgnoresTheStampedSnapshotClock(t *testing.T) {
	// The keepalive dedupe rests on this: two snapshots of identical state
	// differ in updated_at by construction, because the daemon stamps it on
	// every read.
	first := state.Snapshot{SchemaVersion: state.CurrentSchemaVersion, UpdatedAt: testNow,
		Sessions: []state.Session{{PID: 1, StartedAt: testNow}}}
	second := first
	second.UpdatedAt = testNow.Add(time.Hour)
	if !state.ObservablyEqual(first, second) {
		t.Fatal("updated_at alone made two identical snapshots compare unequal")
	}
	third := first
	third.Sessions = []state.Session{{PID: 1, StartedAt: testNow, Suspended: true}}
	if state.ObservablyEqual(first, third) {
		t.Fatal("a real state change compared equal")
	}
	if !reflect.DeepEqual(first.Sessions[0].PID, 1) {
		t.Fatal("fixture drift")
	}
}

// syncBuffer lets the test goroutine read what StreamLocal has written so far
// without racing its writer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}
