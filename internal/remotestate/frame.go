// Package remotestate carries read-only, full Switchboard snapshots from remote
// hosts. It deliberately does not merge them into state.Store and has no remote
// action channel.
package remotestate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tjmisko/switchboard/internal/rpc"
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
	// ErrTruncatedFrame is a transport failure rather than a protocol verdict:
	// the stream ended after a non-empty frame prefix but before its newline.
	ErrTruncatedFrame    = errors.New("remote snapshot frame truncated")
	ErrHostnameChanged   = errors.New("remote source changed hostname")
	ErrDuplicateHost     = errors.New("remote hostname already claimed")
	ErrLocalHostname     = errors.New("remote source claimed local hostname")
	ErrManagerAlreadyRun = errors.New("remote source manager already run")
)

// Frame is one independently meaningful, complete replacement snapshot for a
// canonical host. Frames are encoded as one bounded JSON object per line.
type Frame struct {
	Host     string         `json:"host"`
	Snapshot state.Snapshot `json:"snapshot"`
}

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
	if err := validateSnapshot(frame.Snapshot); err != nil {
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
		Host     string          `json:"host"`
		Snapshot json.RawMessage `json:"snapshot"`
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
	if len(envelope.Snapshot) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Snapshot), []byte("null")) {
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
	if err := validateSnapshot(snapshot); err != nil {
		return Frame{}, err
	}
	return Frame{Host: canonical, Snapshot: snapshot}, nil
}

func validateSnapshot(snapshot state.Snapshot) error {
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
// ceiling for peer-controlled input. Only newline-terminated frames are
// decoded: a non-empty final prefix is ErrTruncatedFrame so the manager can
// distinguish a cut transport from a malformed complete document.
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
		if !complete && len(line) > 0 {
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

// SubscriptionClient is the narrow local RPC surface needed by StreamLocal.
// rpc.Client satisfies it; tests can provide a finite in-memory subscription.
type SubscriptionClient interface {
	Send(rpc.Request) error
	Recv(*rpc.Response) error
}

// StreamOptions configures StreamLocal. OnAttach is called once before the
// subscription request and is the only binding re-announcement hook. This
// package intentionally supplies no binding implementation; switchboard-ctl
// uses the hook to complete the daemon's announce-bindings RPC on the same
// connection before subscribe takes over that connection.
type StreamOptions struct {
	Hostname      func() (string, error)
	OnAttach      func(context.Context) error
	MaxFrameBytes int
}

// StreamLocal subscribes to the daemon on the same machine as the caller and
// emits read-only complete snapshots. It never accepts an SSH destination and
// never forwards an RPC command channel; callers are responsible for dialing
// their normal local daemon socket before entering this function.
func StreamLocal(ctx context.Context, client SubscriptionClient, out io.Writer, options StreamOptions) error {
	hostname := options.Hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	rawHost, err := hostname()
	if err != nil {
		return fmt.Errorf("read local hostname: %w", err)
	}
	host, err := CanonicalHostname(rawHost)
	if err != nil {
		return fmt.Errorf("canonicalize local hostname: %w", err)
	}
	if _, err := frameLimit(options.MaxFrameBytes); err != nil {
		return err
	}
	if options.OnAttach != nil {
		if err := options.OnAttach(ctx); err != nil {
			return fmt.Errorf("announce local bindings: %w", err)
		}
	}
	if err := client.Send(rpc.Request{Cmd: "subscribe"}); err != nil {
		return fmt.Errorf("subscribe local daemon: %w", err)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var response rpc.Response
		if err := client.Recv(&response); err != nil {
			return fmt.Errorf("receive local snapshot: %w", err)
		}
		if response.Error != "" {
			return errors.New("local daemon rejected subscription")
		}
		if response.Snapshot == nil {
			return errors.New("local daemon sent no snapshot")
		}
		if err := validateSnapshot(*response.Snapshot); err != nil {
			return fmt.Errorf("local daemon snapshot: %w", err)
		}
		if err := EncodeFrame(out, Frame{Host: host, Snapshot: *response.Snapshot}, options.MaxFrameBytes); err != nil {
			return fmt.Errorf("write remote stream: %w", err)
		}
	}
}
