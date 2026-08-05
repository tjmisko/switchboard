package main

import (
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// signalSample is one session's transcript reads for the two status self-heals,
// taken before the store lock, plus the state they were taken against.
//
// The self-heals are the last readers left inside the Apply, and they are the
// hottest of the lot: selfHealStuckStatus tail-reads the transcript of EVERY
// idle-or-working session whose file has moved since the chip transitioned, which
// during an active turn is every session, every tick.
//
// They are also the most delicate code in the daemon — the H1/H7/H8/H9 timing
// hazards and the §5 permission case table all live here — so the reads move out
// while every decision stays exactly where it was. A sample is applied only if
// the session still looks the way it did when the read was taken; otherwise the
// self-heal reads inline, precisely as it always did.
type signalSample struct {
	// The state the reads were taken against. If either has moved by the time the
	// lock is held, the sample describes a session that no longer exists in that
	// form and is discarded.
	status      string
	statusSince time.Time
	transcript  string

	// selfHealStuckStatus inputs. quiescent records the cheap stat gate: nothing
	// written since the chip transitioned, so no signal can be newer than it.
	quiescent bool
	kind      transcript.Signal
	kindTs    time.Time
	kindErr   bool

	// selfHealStaleAttention input, taken only for a chip currently latched red.
	resolution    transcript.ResolutionKind
	resolutionErr bool
	resolved      bool

	valid bool
}

// freshFor reports whether this sample still describes the session in hand. The
// guard is exact rather than approximate: both self-heals key their decisions on
// StatusSince, and a status edge landing between the sample and the lock (a hook
// firing mid-tick) changes what the read means, not merely how fresh it is.
func (s signalSample) freshFor(c *state.AgentInfo) bool {
	return s.valid && s.status == c.Status && s.statusSince.Equal(c.StatusSince) && s.transcript == c.Transcript
}

// sampleSignals performs both self-heals' transcript reads for every session in
// the tick's pre-lock snapshot, holding no store lock.
//
// The snapshot is taken by the caller and shared with every other sampler — one
// read lock and one set of session copies per tick, and one view of the world for
// all of them to agree on.
func sampleSignals(snap state.Snapshot, tun statustune.Tuning) map[int]signalSample {
	out := map[int]signalSample{}
	for _, sess := range snap.Sessions {
		c := sess.Claude
		if c == nil || c.Transcript == "" {
			continue
		}
		out[sess.PID] = readSignals(c, tun)
	}
	return out
}

// readSignals is the I/O, shared by the pre-lock sampler and the under-lock
// fallback so the two cannot drift. It takes no locks and mutates nothing.
func readSignals(c *state.AgentInfo, tun statustune.Tuning) signalSample {
	s := signalSample{
		status: c.Status, statusSince: c.StatusSince, transcript: c.Transcript, valid: true,
	}

	if c.Status == state.StatusPermission {
		kind, err := transcript.ResolveKind(c.Transcript, c.StatusSince, tun.TailBytes)
		s.resolution, s.resolutionErr, s.resolved = kind, err != nil, true
		return s // a red chip is never also a stuck idle/working chip
	}

	if c.Status != state.StatusIdle && c.Status != state.StatusWorking {
		return s
	}
	// The cheap stat short-circuit, preserved exactly: if nothing was written since
	// the chip transitioned, no signal can be newer than it and the tail read is
	// skipped. This is what keeps a quiescent box off the disk entirely.
	fi, err := os.Stat(c.Transcript)
	if err != nil || !fi.ModTime().After(c.StatusSince) {
		s.quiescent = true
		return s
	}
	kind, ts, err := transcript.NewestSignal(c.Transcript, tun.TailBytes)
	s.kind, s.kindTs, s.kindErr = kind, ts, err != nil
	return s
}
