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
	// The state the reads were taken against. If any of it has moved by the time
	// the lock is held, the sample describes a session that no longer exists in
	// that form and is discarded. The stamp covers the FILE the reads came from;
	// see freshFor for why the other three are not enough.
	status      string
	statusSince time.Time
	transcript  string
	stamp       fileStamp

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
//
// The session fields alone are NOT enough, which is subtle and was got wrong the
// first time. The thing both self-heals actually read is the transcript, and the
// user can move it without moving any of them: answering or declining a permission
// prompt appends to the file while Status stays "permission", StatusSince stays
// put (nothing has released the chip — that is what the self-heal is FOR), and the
// path is unchanged. The pre-lock window spans every sampler in the tick, so this
// is not a narrow race. Without the stamp the sample computed before the answer
// landed is applied after it, and the red chip stays red for another full tick;
// the same window makes an interrupt notice miss the working→idle recovery.
//
// So re-stamp the file here and require it unchanged. That is one os.Stat per
// sampled session under the lock — deliberate, and cheap next to what this code
// used to do there (a stat AND a bounded tail read, per session, every tick).
func (s signalSample) freshFor(c *state.AgentInfo) bool {
	if !s.valid || s.status != c.Status || !s.statusSince.Equal(c.StatusSince) || s.transcript != c.Transcript {
		return false
	}
	return s.stamp.equal(stampOf(c.Transcript))
}

// fileStamp identifies a transcript's contents cheaply enough to re-check under
// the lock. ok records whether the stat succeeded, so an unreadable file is a
// distinct state rather than a coincidence of zero values; two unreadable stamps
// compare equal, since sampled-missing and still-missing is genuinely unchanged
// and both self-heals already treat a failed read as no signal.
type fileStamp struct {
	modTime time.Time
	size    int64
	ok      bool
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{modTime: fi.ModTime(), size: fi.Size(), ok: true}
}

func (s fileStamp) equal(o fileStamp) bool {
	return s.ok == o.ok && s.size == o.size && s.modTime.Equal(o.modTime)
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
	// Stamped BEFORE the reads below, never after: a write landing between the
	// stamp and the read then makes the stamp look stale, and the sample is
	// discarded. Stamping afterwards would hide exactly that write — the stamp
	// would still match at apply time while the content predates it.
	s.stamp = stampOf(c.Transcript)

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
	// skipped. This is what keeps a quiescent box off the disk entirely. It reuses
	// the stamp above rather than stat-ing twice; a zero stamp is the unreadable
	// case the old code folded into the same branch.
	if !s.stamp.ok || !s.stamp.modTime.After(c.StatusSince) {
		s.quiescent = true
		return s
	}
	kind, ts, err := transcript.NewestSignal(c.Transcript, tun.TailBytes)
	s.kind, s.kindTs, s.kindErr = kind, ts, err != nil
	return s
}
