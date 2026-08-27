// Package remotestate carries read-only, full Switchboard snapshots from remote
// hosts. It deliberately does not merge them into state.Store and has no remote
// action channel.
package remotestate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tjmisko/switchboard/internal/state"
)

const (
	// DefaultMaxFrameBytes is a hard ceiling for one JSON object, excluding its
	// trailing newline. A complete snapshot is normally much smaller; the roomy
	// ceiling avoids turning an unusually busy host into a protocol failure while
	// still bounding allocations controlled by an SSH peer.
	DefaultMaxFrameBytes = 4 << 20
	maxHostnameBytes     = 253
)

var (
	ErrFrameTooLarge  = errors.New("remote snapshot frame too large")
	ErrInvalidFrame   = errors.New("invalid remote snapshot frame")
	ErrSchemaMismatch = errors.New("remote snapshot schema mismatch")
	// ErrTruncatedFrame is a TRANSPORT failure wearing a parser's clothes: the
	// stream ended part-way through a line. It is separated from ErrInvalidFrame
	// because the two must be answered differently — a peer that speaks a
	// protocol we cannot read is dropped at once, while a cut mid-frame is
	// exactly the flaky link whose last observation the client should hold.
	ErrTruncatedFrame    = errors.New("remote snapshot frame truncated")
	ErrHostnameChanged   = errors.New("remote source changed hostname")
	ErrDuplicateHost     = errors.New("remote hostname already claimed")
	ErrLocalHostname     = errors.New("remote source claimed local hostname")
	ErrManagerAlreadyRun = errors.New("remote source manager already run")
	// ErrCloseout ends a read loop on a deliberate peer teardown. It is not a
	// failure: it is the peer's statement that its last frame is final and the
	// client should stop holding rows for it.
	ErrCloseout = errors.New("remote source closed the stream")
)

// Frame is one independently meaningful message about a canonical host,
// encoded as one bounded JSON object per line. It carries exactly one of:
//
//	Snapshot — a complete replacement snapshot (the ordinary case);
//	Closeout — the peer is deliberately ending the stream.
//
// Both older and newer readers stay usable across this addition. A frame with a
// snapshot is byte-identical to the original v1 shape apart from the optional
// keepalive advertisement, which an older reader ignores as an unknown field. A
// closeout frame has no snapshot, so an older reader rejects it as an invalid
// frame and tears the stream down — which is exactly what a closeout asks it to
// do, one log category off.
type Frame struct {
	Host string `json:"host"`
	// Snapshot is nil on a closeout frame and non-nil on every other frame.
	Snapshot *state.Snapshot `json:"snapshot,omitempty"`
	// Closeout is non-nil only on the final frame of a deliberate teardown.
	Closeout *Closeout `json:"closeout,omitempty"`
	// KeepaliveSeconds is the peer ADVERTISING that it re-sends its current
	// snapshot at least this often even when nothing changes. It is what lets a
	// client distinguish "idle host, healthy stream" from "TCP black hole"
	// without waiting for SSH's own keepalive to give up, and it is carried on
	// every snapshot frame so a reconnect re-establishes it with no handshake —
	// which matters because the transport is one-way (ssh -n) and there is no
	// back-channel to negotiate on.
	//
	// Zero means the peer makes no such promise (an older remote), and a client
	// must then fall back to transport-level detection alone.
	KeepaliveSeconds int `json:"keepalive_seconds,omitempty"`
}

// Closeout is a peer's statement that it is going away on purpose, so its last
// snapshot is final and must not be held.
//
// It is emitted for a DELIBERATE teardown of the streaming process only — the
// remote being told to stop, which is what a host shutdown and a manual stop
// both look like. It is deliberately NOT emitted when the remote's own daemon
// socket drops: sessions on that machine are still running, the client is still
// meant to be observing them, and holding the last observation across the
// reconnect is the whole point.
//
// The design invariant is that a closeout can only ever restore the pre-hold
// behavior (drop now, reconnect if you can). It can never remove rows that
// would otherwise have survived, so a peer that emits one too eagerly is no
// worse than a peer that cannot emit one at all.
type Closeout struct {
	// Reason is a short, finite, machine-readable token. It reaches a log line,
	// so it is validated to a strict character set rather than trusted: a peer
	// must not be able to forge log records through it. An unrecognized token is
	// still a closeout — the drop decision must not depend on the client having
	// heard of the reason.
	Reason string `json:"reason,omitempty"`
}

// Closeout reasons emitted by this implementation.
const (
	// CloseoutSignal: the streaming process was told to stop (SIGTERM/SIGINT/
	// SIGHUP). On a host going down this is the last thing that can be said
	// while the link is still up, and it is what makes shutdown responsive.
	CloseoutSignal = "signal"
)

const maxCloseoutReasonBytes = 32

// validateCloseoutReason keeps peer-controlled text out of logs unless it is a
// bare lowercase token. An empty reason is allowed and means "unspecified".
func validateCloseoutReason(reason string) error {
	if len(reason) > maxCloseoutReasonBytes {
		return fmt.Errorf("%w: closeout reason length", ErrInvalidFrame)
	}
	for i := 0; i < len(reason); i++ {
		c := reason[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return fmt.Errorf("%w: closeout reason character", ErrInvalidFrame)
	}
	return nil
}

// MaxKeepaliveSeconds bounds a peer's advertised keepalive period. A peer that
// promised an hour would effectively disable the client's silence detection
// while still looking like it had made a promise, so an out-of-range value is
// a protocol error rather than a clamp.
const MaxKeepaliveSeconds = 300

// CanonicalHostname normalizes the case-insensitive hostname returned by the
// remote OS. SSH aliases are intentionally not accepted here: the host field is
// an assertion by the machine that owns the local daemon, not configuration
// supplied by the client.
func CanonicalHostname(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if host == "" || len(host) > maxHostnameBytes {
		return "", fmt.Errorf("%w: hostname length", ErrInvalidFrame)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("%w: hostname label", ErrInvalidFrame)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
				continue
			}
			return "", fmt.Errorf("%w: hostname character", ErrInvalidFrame)
		}
	}
	return host, nil
}

// EncodeFrame writes exactly one bounded JSONL frame. It marshals before
// writing so an oversized snapshot can never leave a truncated prefix on the
// transport.
func EncodeFrame(w io.Writer, frame Frame, maxBytes int) error {
	limit, err := frameLimit(maxBytes)
	if err != nil {
		return err
	}
	canonical, err := CanonicalHostname(frame.Host)
	if err != nil || canonical != frame.Host {
		return fmt.Errorf("%w: non-canonical hostname", ErrInvalidFrame)
	}
	if err := validateFrameBody(frame); err != nil {
		return err
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode remote snapshot: %w", err)
	}
	if len(body) > limit {
		return ErrFrameTooLarge
	}
	body = append(body, '\n')
	n, err := w.Write(body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	return err
}

// DecodeFrame validates one JSON object (without its newline), including the
// current state schema and all fields that distinguish a complete snapshot
// from a syntactically valid partial document.
func DecodeFrame(body []byte) (Frame, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return Frame{}, fmt.Errorf("%w: empty line", ErrInvalidFrame)
	}
	var envelope struct {
		Host             string          `json:"host"`
		Snapshot         json.RawMessage `json:"snapshot"`
		Closeout         *Closeout       `json:"closeout"`
		KeepaliveSeconds int             `json:"keepalive_seconds"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&envelope); err != nil {
		return Frame{}, fmt.Errorf("%w: envelope", ErrInvalidFrame)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Frame{}, fmt.Errorf("%w: trailing data", ErrInvalidFrame)
	}
	canonical, err := CanonicalHostname(envelope.Host)
	if err != nil || canonical != envelope.Host {
		return Frame{}, fmt.Errorf("%w: non-canonical hostname", ErrInvalidFrame)
	}
	if envelope.KeepaliveSeconds < 0 || envelope.KeepaliveSeconds > MaxKeepaliveSeconds {
		return Frame{}, fmt.Errorf("%w: keepalive out of range", ErrInvalidFrame)
	}
	hasSnapshot := len(envelope.Snapshot) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Snapshot), []byte("null"))
	if envelope.Closeout != nil {
		// A closeout is terminal, so nothing after it in this frame can matter.
		// Rejecting a snapshot alongside it keeps "exactly one of" true on the
		// wire and denies a peer any way to make the two disagree.
		if hasSnapshot {
			return Frame{}, fmt.Errorf("%w: closeout carries a snapshot", ErrInvalidFrame)
		}
		if err := validateCloseoutReason(envelope.Closeout.Reason); err != nil {
			return Frame{}, err
		}
		return Frame{Host: canonical, Closeout: &Closeout{Reason: envelope.Closeout.Reason}}, nil
	}
	if !hasSnapshot {
		return Frame{}, fmt.Errorf("%w: missing snapshot", ErrInvalidFrame)
	}

	var required map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Snapshot, &required); err != nil {
		return Frame{}, fmt.Errorf("%w: snapshot object", ErrInvalidFrame)
	}
	for _, name := range []string{"schema_version", "sessions", "updated_at"} {
		value, ok := required[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Frame{}, fmt.Errorf("%w: incomplete snapshot", ErrInvalidFrame)
		}
	}

	var snapshot state.Snapshot
	if err := json.Unmarshal(envelope.Snapshot, &snapshot); err != nil {
		return Frame{}, fmt.Errorf("%w: snapshot", ErrInvalidFrame)
	}
	if err := validateSnapshot(&snapshot); err != nil {
		return Frame{}, err
	}
	return Frame{Host: canonical, Snapshot: &snapshot, KeepaliveSeconds: envelope.KeepaliveSeconds}, nil
}

// validateFrameBody enforces the "exactly one of snapshot/closeout" rule and
// whichever payload is present. It is shared by the encoder and by Manager's
// injected-frame path so a frame built in memory faces the same rules as one
// that arrived over the wire.
func validateFrameBody(frame Frame) error {
	if frame.KeepaliveSeconds < 0 || frame.KeepaliveSeconds > MaxKeepaliveSeconds {
		return fmt.Errorf("%w: keepalive out of range", ErrInvalidFrame)
	}
	if frame.Closeout != nil {
		if frame.Snapshot != nil {
			return fmt.Errorf("%w: closeout carries a snapshot", ErrInvalidFrame)
		}
		return validateCloseoutReason(frame.Closeout.Reason)
	}
	return validateSnapshot(frame.Snapshot)
}

func validateSnapshot(snapshot *state.Snapshot) error {
	if snapshot == nil {
		return fmt.Errorf("%w: missing snapshot", ErrInvalidFrame)
	}
	if snapshot.SchemaVersion != state.CurrentSchemaVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrSchemaMismatch, snapshot.SchemaVersion, state.CurrentSchemaVersion)
	}
	if snapshot.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: missing updated_at", ErrInvalidFrame)
	}
	seen := make(map[int]struct{}, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		if session.PID <= 0 || session.StartedAt.IsZero() {
			return fmt.Errorf("%w: incomplete session identity", ErrInvalidFrame)
		}
		if _, duplicate := seen[session.PID]; duplicate {
			return fmt.Errorf("%w: duplicate session pid", ErrInvalidFrame)
		}
		seen[session.PID] = struct{}{}
	}
	return nil
}

func frameLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultMaxFrameBytes, nil
	}
	if limit < 0 || limit > DefaultMaxFrameBytes {
		return 0, fmt.Errorf("frame limit must be between 1 and %d bytes", DefaultMaxFrameBytes)
	}
	return limit, nil
}

// FrameReader is the injectable reader seam used by Manager. ReadFrames is the
// production implementation.
type FrameReader func(io.Reader, int, func(Frame) error) error

// ReadFrames reads bounded JSONL objects until EOF or until validation or the
// callback rejects a frame. It never allocates beyond the configured line
// ceiling for peer-controlled input.
//
// Only COMPLETE lines are decoded. A non-empty tail with no terminating newline
// means the stream was cut part-way through a frame, and reporting that as a
// malformed frame would libel the peer: EncodeFrame marshals whole and always
// terminates, so an unterminated tail is the transport's doing, never the
// encoder's. The distinction is load-bearing downstream — a malformed frame
// drops the host at once, a cut link holds its last observation.
func ReadFrames(r io.Reader, maxBytes int, accept func(Frame) error) error {
	limit, err := frameLimit(maxBytes)
	if err != nil {
		return err
	}
	reader := bufio.NewReaderSize(r, limit+1)
	for {
		line, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) || len(line) > limit+1 {
			return ErrFrameTooLarge
		}
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		if !complete && len(bytes.TrimSpace(line)) > 0 {
			// Report the truncation, not whatever the partial JSON happens to
			// parse as. Any read outcome is subordinate to it: the frame this
			// prefix belonged to is lost either way.
			return ErrTruncatedFrame
		}
		if complete {
			line = line[:len(line)-1]
			if len(line) > limit {
				return ErrFrameTooLarge
			}
			frame, err := DecodeFrame(line)
			if err != nil {
				return err
			}
			if err := accept(frame); err != nil {
				return err
			}
		}
		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF):
			return io.EOF
		default:
			return readErr
		}
	}
}
