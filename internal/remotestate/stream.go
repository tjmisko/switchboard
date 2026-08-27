package remotestate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

// DefaultKeepalive is how often a quiet stream re-sends its current snapshot.
//
// The local daemon publishes only on a real change (state.snapshotChangeKey),
// so an idle host emits nothing for minutes and a reader cannot tell a healthy
// quiet stream from a dead link. SSH's own keepalive answers that eventually;
// this answers it in seconds, and it costs one small frame per host per period
// on an otherwise idle connection.
//
// A keepalive is a verbatim RE-SEND of the last snapshot rather than a new
// message type. That is what makes it invisible to an older client: it sees an
// ordinary frame carrying state it already has, applies it, and its own publish
// gate suppresses the redundant broadcast. There is no protocol version to
// negotiate — which matters, because the transport is one-way and there is
// nowhere to negotiate one.
const DefaultKeepalive = 10 * time.Second

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

	// Keepalive is the quiet-stream re-send period. Zero selects
	// DefaultKeepalive; a negative value disables keepalives entirely, which
	// also withdraws the advertisement so a client does not expect frames that
	// will never come.
	Keepalive time.Duration
	// Closeout delivers a deliberate-teardown reason. The caller owns the
	// trigger — switchboard-ctl wires process signals to it — because installing
	// signal handlers is the command's business, not this package's. Receiving a
	// reason writes one final closeout frame and returns nil: a teardown the
	// operator asked for is not a stream failure.
	Closeout <-chan string
	// Ticker is the timer seam for Keepalive. Tests supply a driven channel so
	// nothing sleeps.
	Ticker func(time.Duration) (<-chan time.Time, func())
}

func (o StreamOptions) keepalive() (time.Duration, bool) {
	switch {
	case o.Keepalive < 0:
		return 0, false
	case o.Keepalive == 0:
		return DefaultKeepalive, true
	default:
		return o.Keepalive, true
	}
}

func systemTicker(period time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(period)
	return ticker.C, ticker.Stop
}

// StreamLocal subscribes to the daemon on the same machine as the caller and
// emits read-only complete snapshots. It never accepts an SSH destination and
// never forwards an RPC command channel; callers are responsible for dialing
// their normal local daemon socket before entering this function.
//
// Every write to out happens on this goroutine. The subscription is drained by
// a helper so a quiet daemon cannot block the keepalive and a keepalive cannot
// interleave bytes with a snapshot mid-frame.
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
	period, keepaliveEnabled := options.keepalive()
	if keepaliveEnabled && (period < time.Second || period > MaxKeepaliveSeconds*time.Second) {
		return fmt.Errorf("keepalive must be between 1s and %ds", MaxKeepaliveSeconds)
	}
	advertised := 0
	if keepaliveEnabled {
		advertised = int(period / time.Second)
	}
	if options.OnAttach != nil {
		if err := options.OnAttach(ctx); err != nil {
			return fmt.Errorf("announce local bindings: %w", err)
		}
	}
	if err := client.Send(rpc.Request{Cmd: "subscribe"}); err != nil {
		return fmt.Errorf("subscribe local daemon: %w", err)
	}

	snapshots, receiveErr := receiveSnapshots(ctx, client)

	var beat <-chan time.Time
	stopTicker := func() {}
	if keepaliveEnabled {
		newTicker := options.Ticker
		if newTicker == nil {
			newTicker = systemTicker
		}
		beat, stopTicker = newTicker(period)
	}
	defer stopTicker()

	write := func(frame Frame) error {
		if err := EncodeFrame(out, frame, options.MaxFrameBytes); err != nil {
			return fmt.Errorf("write remote stream: %w", err)
		}
		return nil
	}

	var latest *state.Snapshot
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case reason := <-options.Closeout:
			// Best effort by construction: on the common teardown path (the SSH
			// client hanging up) stdout is already broken and this write fails.
			// It is worth attempting anyway because the case it DOES reach — a
			// host being shut down while the link is still up — is precisely the
			// one where waiting out a hold window would feel broken.
			_ = write(Frame{Host: host, Closeout: &Closeout{Reason: reason}})
			return nil
		case <-beat:
			if latest == nil {
				continue
			}
			if err := write(Frame{Host: host, Snapshot: latest, KeepaliveSeconds: advertised}); err != nil {
				return err
			}
		case snapshot, ok := <-snapshots:
			if !ok {
				return <-receiveErr
			}
			latest = snapshot
			if err := write(Frame{Host: host, Snapshot: latest, KeepaliveSeconds: advertised}); err != nil {
				return err
			}
		}
	}
}

// receiveSnapshots drains the local subscription onto a channel. It exists so
// the writer above can serve a keepalive while the daemon is quiet; the error
// channel carries the single terminal reason the subscription ended.
func receiveSnapshots(ctx context.Context, client SubscriptionClient) (<-chan *state.Snapshot, <-chan error) {
	snapshots := make(chan *state.Snapshot)
	failure := make(chan error, 1)
	go func() {
		defer close(snapshots)
		for {
			var response rpc.Response
			if err := client.Recv(&response); err != nil {
				failure <- fmt.Errorf("receive local snapshot: %w", err)
				return
			}
			if response.Error != "" {
				failure <- errors.New("local daemon rejected subscription")
				return
			}
			if response.Snapshot == nil {
				failure <- errors.New("local daemon sent no snapshot")
				return
			}
			if err := validateSnapshot(response.Snapshot); err != nil {
				failure <- fmt.Errorf("local daemon snapshot: %w", err)
				return
			}
			select {
			case snapshots <- response.Snapshot:
			case <-ctx.Done():
				failure <- ctx.Err()
				return
			}
		}
	}()
	return snapshots, failure
}
