package main

import (
	"errors"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
)

// memorySampler reads every live session's resident memory once per reconcile
// tick — and reads it OUTSIDE the store lock.
//
// The lock, not the CPU, is the constraint. Store.Apply holds an exclusive write
// lock across its whole callback and the snapshot it takes afterwards, so every
// millisecond spent inside it is a millisecond that waybar, ctl, claude-tui and
// every inbound hook spend blocked. A full sampling pass costs 10-20 ms on this
// machine (369 processes, 4 live sessions, 8 processes across their trees):
// ~4-6 ms for the one process-table scan, then ~0.8-1.6 ms per smaps_rollup.
// That second figure scales with the TARGET's VMA count rather than with the
// size of the machine — the kernel computes PSS by walking the mapping list — so
// a 500 MB agent costs far more than the shell beside it, and the total is
// several times the rest of the tick's work put together.
//
// So the sampler runs before the lock is taken, against the pid set of the last
// published snapshot; only the assignment and the sink.Record happen inside
// Apply, which is a map lookup and a non-blocking channel send. Sampling a pid
// that died in between is harmless — the read reports ErrGone and that pid
// yields no sample.
//
// A suspended session is sampled like any other: Ctrl-Z stops a process, it does
// not free a page of it, so its memory is exactly as real as a running one's.
//
// A nil *memorySampler is the disabled form: sample returns an empty tick and
// nothing reads /proc at all.
type memorySampler struct {
	proc *proc.Reader
	// last is the last successful reading per pid, so a transient failure to read
	// a process that is still alive can repeat it rather than flap to zero.
	last map[int]proc.TreeMem
}

// newMemorySampler returns a sampler reading the real /proc.
func newMemorySampler() *memorySampler { return newMemorySamplerAt("") }

// newMemorySamplerAt roots the sampler at a /proc-shaped directory instead
// (testsupport.FakeProcTree), which is what makes tree attribution and the
// gone-process rule testable without spawning real processes.
func newMemorySamplerAt(procRoot string) *memorySampler {
	return &memorySampler{proc: proc.NewReader(procRoot), last: map[int]proc.TreeMem{}}
}

// sessionMem is one session's reading for a tick.
//
// Fresh separates a reading taken this tick from a last-known value repeated
// after a failed read. The two have different right answers: the live state
// wants the repeat (a tooltip that drops to zero on a transient error is worse
// than one that lags a tick), while the durable log must never receive it — on
// disk a repeated gauge is indistinguishable from a genuine flat reading, and
// the fold would treat it as measured.
type sessionMem struct {
	proc.TreeMem
	Fresh bool
}

// memoryTick is one tick's readings: the machine-wide figures, taken once for
// the whole tick, plus each sampled session's tree keyed by pid. The zero value
// is a tick that sampled nothing, and reads from it are safe.
type memoryTick struct {
	Sys      proc.SysMem
	Sessions map[int]sessionMem
}

// sample reads machine-wide pressure once and then each pid's process tree,
// against ONE snapshot of the process table.
//
// pids comes from the PREVIOUS tick's snapshot, because the current session map
// cannot be read without taking the lock this whole function exists to stay out
// of. A session discovered during this tick is therefore first sampled on the
// next one — the same one-tick delay observeUsage's cursor priming produces,
// though for a different reason. Usage primes to avoid dumping a pre-existing
// transcript's backlog as a single spike dated at daemon start; a memory sample
// is an instantaneous gauge with no backlog to dump, so here the delay is simply
// what falls out of reading the pid set before the lock.
func (ms *memorySampler) sample(pids []int) memoryTick {
	if ms == nil || len(pids) == 0 {
		return memoryTick{}
	}
	tick := memoryTick{Sessions: make(map[int]sessionMem, len(pids))}

	// Machine-wide, so it is read once and stamped onto every sample in the tick.
	// A failed read leaves the fields zero and omitempty drops them: absent means
	// "not measured", which is the honest reading of a file that would not open.
	tick.Sys, _ = ms.proc.SystemMemory()

	// ONE process-table scan for the whole tick. TreeMemory would rebuild it per
	// session, and that scan is most of what a sample costs. Sharing it also makes
	// the tick's readings mutually consistent: every process is attributed to
	// exactly one tree even if the kernel reparents it while we walk.
	parents, err := ms.proc.ParentMap()
	if err != nil {
		return tick // /proc unreadable (or not Linux) — no readings this tick
	}
	for _, pid := range pids {
		if mem, ok := ms.read(parents, pid); ok {
			tick.Sessions[pid] = mem
		}
	}
	ms.prune(pids)
	return tick
}

// read returns one session's reading, or ok=false when there is nothing to
// record for this pid this tick.
func (ms *memorySampler) read(parents map[int]int, pid int) (sessionMem, bool) {
	mem, err := ms.proc.TreeMemoryFrom(parents, pid)
	switch {
	case err == nil:
		ms.last[pid] = mem
		return sessionMem{TreeMem: mem, Fresh: true}, true

	case errors.Is(err, proc.ErrGone), errors.Is(err, proc.ErrNoRollup):
		// Nothing to measure: the process exited between the snapshot and now, or
		// it is a zombie, whose rollup is present but empty. Report NO sample
		// rather than a zero one — a zero reads as "the session freed all its
		// memory" and would corrupt both the peak and the time-weighted average.
		delete(ms.last, pid)
		return sessionMem{}, false

	default:
		// A process that is still there but could not be read this once. It holds
		// its pages either way, so keep the last-known figure rather than flapping
		// (the rule the suspension refresh follows on the same loop) — but mark it
		// stale, so it reaches the live state and never the log.
		mem, ok := ms.last[pid]
		return sessionMem{TreeMem: mem}, ok
	}
}

// prune drops cached readings for pids no longer being sampled, so the map does
// not grow without bound as sessions come and go.
func (ms *memorySampler) prune(live []int) {
	if len(ms.last) == 0 {
		return
	}
	keep := make(map[int]struct{}, len(live))
	for _, pid := range live {
		keep[pid] = struct{}{}
	}
	for pid := range ms.last {
		if _, ok := keep[pid]; !ok {
			delete(ms.last, pid)
		}
	}
}

// event builds the memory_sample for one session, or ok=false when this tick
// holds no fresh reading for it. Emitted unconditionally otherwise: the sample
// is a gauge, so unlike usage there is no "nothing changed" case worth skipping.
//
// CWD rides along so the sink can resolve the project abbreviation before
// scrubbing it. Nothing else here is scrubbed at the minimal tier, deliberately:
// every mem_*/sys_* field is a byte or microsecond count describing how much,
// and the tree is reported as one summed figure plus a process count — no child
// names, no per-process breakdown, nothing content-shaped.
func (t memoryTick) event(sess *state.Session, now time.Time) (history.Event, bool) {
	mem, ok := t.Sessions[sess.PID]
	if !ok || !mem.Fresh {
		return history.Event{}, false
	}
	ev := history.Event{
		Ts: now, Type: history.EventMemorySample,
		SessionID: enrichmentID(sess), PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		MemAgentPssBytes:  mem.Agent.Pss,
		MemAgentSwapBytes: mem.Agent.SwapPss,
		MemTreePssBytes:   mem.Tree.Pss,
		MemTreeSwapBytes:  mem.Tree.SwapPss,
		MemTreeProcs:      mem.Procs,
		SysAvailBytes:     t.Sys.AvailBytes,
	}
	// A kernel built without CONFIG_PSI writes no PSI fields at all rather than a
	// row of zeroes: absent means "not measured" and zero means "measured, and no
	// stall", and OOM forensics turns on the difference.
	if t.Sys.PSI.Present {
		ev.SysPsiSomeAvg10 = t.Sys.PSI.Avg10
		ev.SysPsiSomeTotalUs = t.Sys.PSI.TotalUS
	}
	return ev, true
}

// applyLocked is the ENTIRE under-lock half of the memory sampler: write the
// pre-lock reading onto the session, then log a fresh one. It does no I/O, and
// that is the property the sampler contract in cmd/switchboard pins.
//
// It is a named function rather than four lines inline in reconcileOnce so the
// contract can drive the real thing. Registered as a copy of those lines, the
// contract would keep passing over its own stale duplicate on the day someone
// adds a read here — which is the one day it exists for. Every other registration
// already drives a real production function (observeFanout, selfHealStuckStatus,
// observeLabel, sweepDeadSessions); this is what gives the memory one the same
// lever.
//
// The live fields take whatever the tick has, INCLUDING a repeated last-known
// figure after a failed read: better a stale tooltip than one that flaps to zero.
// The log takes only a fresh reading (event's own Fresh gate), so a process that
// is gone yields NO sample rather than a zero one — a zero would read as "freed
// all its memory" and corrupt the peak and average.
func (t memoryTick) applyLocked(sess *state.Session, sink *history.Sink, now time.Time) {
	if reading, ok := t.Sessions[sess.PID]; ok {
		sess.MemAgentBytes = reading.Agent.Pss
		sess.MemTreeBytes = reading.Tree.Pss
	}
	if ev, ok := t.event(sess, now); ok {
		sink.Record(ev)
	}
}

// sampleMemory takes the tick's readings before the reconciler locks the store.
// A no-op when memory sampling is off.
func (rs *reconcileState) sampleMemory(snap state.Snapshot) memoryTick {
	if rs.memory == nil {
		return memoryTick{}
	}
	return rs.memory.sample(livePIDs(snap))
}

// livePIDs is the pid set a tick samples: every session in the snapshot the tick
// took before locking. Read through Snapshot — a read lock, shared with every
// other reader — rather than through Apply, which is the exclusive lock the
// sampler exists to stay out of.
func livePIDs(snap state.Snapshot) []int {
	pids := make([]int, 0, len(snap.Sessions))
	for _, sess := range snap.Sessions {
		pids = append(pids, sess.PID)
	}
	return pids
}
