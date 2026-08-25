package panebind

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestOSCSequenceUsesFixedVariableAndStandardBase64(t *testing.T) {
	key := exact("buildbox", 1234, "2026-08-24T20:00:00Z")
	payload := mustEncode(t, key)
	sequence, err := OSCSequence(payload)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("\x1b]1337;SetUserVar=" + VariableName + "=")
	if !bytes.HasPrefix(sequence, prefix) || sequence[len(sequence)-1] != '\a' || len(sequence) > MaxOSCBytes {
		t.Fatalf("bad OSC framing: %q", sequence)
	}
	clearEnd := len(prefix)
	if sequence[clearEnd] != '\a' || !bytes.HasPrefix(sequence[clearEnd+1:], prefix) {
		t.Fatalf("sequence does not clear then set: %q", sequence)
	}
	setValue := clearEnd + 1 + len(prefix)
	decoded, err := base64.StdEncoding.DecodeString(string(sequence[setValue : len(sequence)-1]))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	got, err := Decode(string(decoded))
	if err != nil || !got.Equal(key) {
		t.Fatalf("decoded OSC payload = (%+v, %v), want %+v", got, err, key)
	}
}

func TestOSCSequenceAlwaysClearThenSetsRepeatedValue(t *testing.T) {
	payload := mustEncode(t, exact("h", 1, "2026-08-24T20:00:00Z"))
	first, err := OSCSequence(payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OSCSequence(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || bytes.Count(first, []byte(oscPrefix)) != 2 {
		t.Fatalf("repeat was not a deterministic clear-set pair")
	}
}

func TestOSCSequenceRejectsPayloadRatherThanRelayingOpaqueText(t *testing.T) {
	if _, err := OSCSequence("remote-controlled text"); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("error = %v, want malformed payload", err)
	}
}

type recordingTTY struct {
	bytes.Buffer
	writes int
	closed bool
	limit  int
	err    error
}

func (w *recordingTTY) Write(p []byte) (int, error) {
	w.writes++
	if w.err != nil {
		return 0, w.err
	}
	if w.limit > 0 && len(p) > w.limit {
		p = p[:w.limit]
	}
	return w.Buffer.Write(p)
}

func (w *recordingTTY) Close() error { w.closed = true; return nil }

func TestAnnounceOpensThenRevalidatesThenWritesOnce(t *testing.T) {
	key := exact("buildbox", 1234, "2026-08-24T20:00:00-07:00")
	tty := &recordingTTY{}
	opened := false
	validated := false
	a := Announcer{OpenTTY: func(path string) (io.WriteCloser, error) {
		if path != "/dev/pts/7" {
			t.Fatalf("open path = %q", path)
		}
		opened = true
		return tty, nil
	}}
	err := a.Announce(t.Context(), Target{Session: key, TTY: "/dev/pts/7"}, func(got ExactSessionKey, path string) bool {
		if !opened {
			t.Fatal("validator ran before the tty was opened")
		}
		validated = true
		return got == key.Canonical() && path == "/dev/pts/7"
	})
	if err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if !validated || tty.writes != 1 || !tty.closed {
		t.Fatalf("validated=%t writes=%d closed=%t", validated, tty.writes, tty.closed)
	}
	if !strings.Contains(tty.String(), "SetUserVar="+VariableName+"=") {
		t.Fatalf("write = %q", tty.String())
	}
}

func TestAnnounceStaleTargetClosesWithoutWriting(t *testing.T) {
	tty := &recordingTTY{}
	a := Announcer{OpenTTY: func(string) (io.WriteCloser, error) { return tty, nil }}
	err := a.Announce(t.Context(), Target{
		Session: exact("h", 1, "2026-08-24T20:00:00Z"), TTY: "/dev/pts/1",
	}, func(ExactSessionKey, string) bool { return false })
	if !errors.Is(err, ErrStaleEmitTarget) {
		t.Fatalf("error = %v, want stale target", err)
	}
	if tty.writes != 0 || !tty.closed {
		t.Fatalf("writes=%d closed=%t, want 0,true", tty.writes, tty.closed)
	}
}

func TestAnnounceRequiresValidatorAndHonorsCancelledContextBeforeOpen(t *testing.T) {
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	target := Target{Session: key, TTY: "/dev/pts/1"}
	a := Announcer{OpenTTY: func(string) (io.WriteCloser, error) {
		t.Fatal("opener must not run")
		return nil, nil
	}}
	if err := a.Announce(t.Context(), target, nil); !errors.Is(err, ErrNoLiveValidator) {
		t.Fatalf("nil validator error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.Announce(ctx, target, func(ExactSessionKey, string) bool { return true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
}

func TestAnnounceRejectsPartialWrite(t *testing.T) {
	tty := &recordingTTY{limit: 1}
	a := Announcer{OpenTTY: func(string) (io.WriteCloser, error) { return tty, nil }}
	err := a.Announce(t.Context(), Target{
		Session: exact("h", 1, "2026-08-24T20:00:00Z"), TTY: "/dev/pts/1",
	}, func(ExactSessionKey, string) bool { return true })
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want short write", err)
	}
	if got := tty.Bytes(); len(got) == 0 || got[len(got)-1] != '\a' {
		t.Fatalf("partial OSC was not terminated: %q", got)
	}
}
