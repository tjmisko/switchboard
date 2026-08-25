package panebind

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
)

const oscPrefix = "\x1b]1337;SetUserVar=" + VariableName + "="

// MaxOSCBytes includes a clear assignment and a set assignment. The clear
// makes a repeated announcement observable even on builds that coalesce
// unchanged values.
const MaxOSCBytes = 2*len(oscPrefix) + ((MaxPayloadBytes+2)/3)*4 + 2

const maxWriteAttempts = 8

// Target is the complete set of caller-owned facts required to announce one
// binding. The validator must compare all three session fields and TTY against
// current live state.
type Target struct {
	Session ExactSessionKey
	TTY     string
}

type LiveValidator func(ExactSessionKey, string) bool
type TTYOpener func(string) (io.WriteCloser, error)

// Announcer emits one bounded terminal escape. The default opener uses a
// nonblocking, no-follow file descriptor and verifies that it is a TTY-like
// character device. OpenTTY is injectable for deterministic tests.
type Announcer struct {
	OpenTTY TTYOpener
}

func NewAnnouncer() Announcer { return Announcer{OpenTTY: openTTY} }

// OSCSequence validates and canonicalizes a v1 payload, then emits a clear
// assignment followed by the set assignment. WezTerm base64-decodes the value
// before passing it to Lua's user-var-changed callback. The Lua integration
// ignores the intentional empty callback.
func OSCSequence(payload string) ([]byte, error) {
	key, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	canonical, err := Encode(key)
	if err != nil {
		return nil, err
	}
	encodedLen := base64.StdEncoding.EncodedLen(len(canonical))
	clearLen := len(oscPrefix) + 1
	out := make([]byte, clearLen+len(oscPrefix)+encodedLen+1)
	copy(out, oscPrefix)
	out[len(oscPrefix)] = '\a'
	copy(out[clearLen:], oscPrefix)
	setValue := clearLen + len(oscPrefix)
	base64.StdEncoding.Encode(out[setValue:setValue+encodedLen], []byte(canonical))
	out[len(out)-1] = '\a'
	if len(out) > MaxOSCBytes {
		return nil, ErrPayloadTooLarge
	}
	return out, nil
}

// Announce opens the target first, then asks the caller to revalidate the exact
// live tuple immediately before the bounded nonblocking write sequence. Holding
// the opened PTY descriptor also prevents its device from being recycled between
// validation and write. Callers treat any returned error as non-fatal.
func (a Announcer) Announce(ctx context.Context, target Target, live LiveValidator) error {
	if live == nil {
		return ErrNoLiveValidator
	}
	if target.TTY == "" {
		return fmt.Errorf("%w: empty tty", ErrInvalidSession)
	}
	payload, err := Encode(target.Session)
	if err != nil {
		return err
	}
	sequence, err := OSCSequence(payload)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	opener := a.OpenTTY
	if opener == nil {
		opener = openTTY
	}
	tty, err := opener(target.TTY)
	if err != nil {
		return fmt.Errorf("panebind: open tty: %w", err)
	}
	defer tty.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !live(target.Session.Canonical(), target.TTY) {
		return ErrStaleEmitTarget
	}
	return writeSequence(tty, sequence)
}

// writeSequence completes ordinary short writes without ever blocking on the
// default O_NONBLOCK descriptor. The attempt cap keeps an injected or unusual
// writer bounded. If an OSC was partially written and cannot be completed, a
// best-effort BEL terminates it so subsequent terminal output is not swallowed.
func writeSequence(w io.Writer, sequence []byte) error {
	written := 0
	var retryErr error
	for attempt := 0; attempt < maxWriteAttempts && written < len(sequence); attempt++ {
		n, err := w.Write(sequence[written:])
		if n < 0 || n > len(sequence)-written {
			if written > 0 {
				_, _ = w.Write([]byte{'\a'})
			}
			return io.ErrShortWrite
		}
		written += n
		if written == len(sequence) {
			return nil
		}
		if err != nil {
			if retryableWriteError(err) {
				retryErr = err
				continue
			}
			if written > 0 && written < len(sequence) {
				_, _ = w.Write([]byte{'\a'})
			}
			return fmt.Errorf("panebind: write tty: %w", err)
		}
		if n == 0 {
			continue
		}
	}
	if written != len(sequence) {
		if written > 0 {
			_, _ = w.Write([]byte{'\a'})
		}
		if retryErr != nil {
			return fmt.Errorf("panebind: write tty: %w", retryErr)
		}
		return io.ErrShortWrite
	}
	return nil
}
