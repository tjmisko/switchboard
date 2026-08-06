package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

// This file registers every sample* reader of the reconcile tick in
// conformance.RunSamplerContract. One registration each, in the order
// reconcileOnce runs them:
//
//	sampleMemory   sampleFanout   sampleProc   sampleLabels   sampleUsage   sampleSignals
//
// Adding a sampler to the tick means adding it here. Nothing enforces that — see
// the honest limit on the suite — which is why the tick budget tests in
// reconcile_lock_test.go exist alongside it, measuring the whole Apply against an
// injected delay without caring which function spent it. Read that claim with the
// bound stated on TestShouldNotHoldTheStoreLockAcrossTheFanoutSeed: a budget is a
// fraction of an injected delay, so it catches an unregistered reader only once
// that reader is EXPENSIVE. Cheap ones are this suite's job, and only a
// registered one is covered.
//
// The tick has two more pre-lock reads that are NOT sample* and are deliberately
// not registered here, stated so the boundary is not mistaken for an oversight:
//
//   - enumerateForResolve, consumed by resolver.ReconcileFrom under the lock. Its
//     no-I/O property is pinned at the source, in internal/mapping, by
//     explodingLocator/explodingManager — fakes that fail the test if ReconcileFrom
//     touches the terminal or the WM at all. That is a stronger statement than
//     proof-by-removal, and it belongs in the package that owns the function.
//   - manager.ActiveWindow, consumed by applyFocus. applyFocus takes the active
//     address as a plain string and holds nothing it could re-read from, so there
//     is no under-lock read for a contract to detect.
//
// A new pre-lock reader that stages a result for the Apply belongs here. One that,
// like these two, hands the Apply an inert value can be pinned where it lives —
// but it has to be pinned somewhere.

// eventDigest renders one history event without its clocks.
//
// The contract compares the answers of independent runs, and every one of these
// events carries a timestamp taken from wall time or a file's mtime — neither of
// which reproduces across two builds of the same fixture. What the contract is
// about is WHAT was read, never when, so the clock fields (Ts, DurPrevMs) are
// deliberately dropped and everything a sampler could plausibly get wrong is kept.
func eventDigest(ev history.Event) string {
	return fmt.Sprintf("%s pid=%d sid=%s agent=%s wf=%s from=%s to=%s rule=%s label=%s type=%s desc=%s model=%s in=%d out=%d memAgent=%d memTree=%d procs=%d",
		ev.Type, ev.PID, ev.SessionID, ev.AgentID, ev.WorkflowRunID, ev.From, ev.To, ev.Rule,
		ev.Label, ev.AgentType, ev.Description, ev.Model, ev.TokIn, ev.TokOut,
		ev.MemAgentPssBytes, ev.MemTreePssBytes, ev.MemTreeProcs)
}

// drainSink closes a sink and returns its recorded events as sorted digests.
//
// Sorted because the sink writes concurrently and a run's events are a SET here,
// not a sequence — ordering between two events recorded in the same instant is
// not what any of these registrations is asserting.
func drainSink(t *testing.T, sink *history.Sink, dir string) []string {
	t.Helper()
	sink.Close()
	var out []string
	for _, ev := range readEvents(t, dir) {
		out = append(out, eventDigest(ev))
	}
	sort.Strings(out)
	return out
}

func newTestSink(t *testing.T) (*history.Sink, string) {
	t.Helper()
	dir := t.TempDir()
	return history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: dir}), dir
}

// liveAndGone are two sibling paths under one temp dir: the fixture is laid out
// in `live`, and Detach renames it to `gone`. Both stay under the dir the test
// framework cleans up, so a detached fixture leaks nothing.
func liveAndGone(t *testing.T) (live, gone string) {
	t.Helper()
	root := t.TempDir()
	live, gone = filepath.Join(root, "live"), filepath.Join(root, "gone")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	return live, gone
}

func renameAway(t *testing.T, live, gone string) {
	t.Helper()
	if err := os.Rename(live, gone); err != nil {
		t.Fatal(err)
	}
}

func storeWith(t *testing.T, sess *state.Session) *state.Store {
	t.Helper()
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) { m[sess.PID] = sess })
	return store
}

// ---------------------------------------------------------------------------
// sampleFanout — the history seed, the transcript cursor, the subagents dir scan
// ---------------------------------------------------------------------------

// The most expensive reader in the tick and the one that regressed: the seed
// alone measured 1.81 s per newly-seen session against a live archive, paid with
// the exclusive store lock held.
func TestSamplerContract_sampleFanout(t *testing.T) {
	conformance.RunSamplerContract(t, conformance.Sampler{
		Name: "sampleFanout",
		Build: func(t *testing.T) conformance.SamplerRun {
			live, gone := liveAndGone(t)
			sid := "s-fanout"
			tpath := filepath.Join(live, sid+".jsonl")
			writeLines(t, tpath, `{"type":"system"}`)

			// One subagent, still running: the dir scan is the authoritative spawn
			// source, so this is the fact the apply phase must get from the sample.
			subdir := filepath.Join(live, sid, "subagents")
			if err := os.MkdirAll(subdir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeLines(t, filepath.Join(subdir, "agent-n1.meta.json"),
				`{"agentType":"Explore","description":"probe","spawnDepth":1,"toolUseId":"toolu_n1"}`)
			writeLines(t, filepath.Join(subdir, "agent-n1.jsonl"),
				`{"type":"assistant","message":{"role":"assistant","stop_reason":null}}`)

			// The archive the first-sight seed reads. It lives INSIDE the detachable
			// tree on purpose: the seed is a read this sampler makes, so the contract
			// has to be able to take it away too.
			histDir := filepath.Join(live, "history")
			if err := os.MkdirAll(histDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeLines(t, filepath.Join(histDir, "2026-06-27.jsonl"),
				`{"ts":"2026-06-27T12:00:00Z","type":"subagent_spawn","session_id":"`+sid+`","agent_id":"r0"}`)

			sess := &state.Session{PID: 11, Agent: state.AgentKindClaude, CWD: "/proj",
				Claude: &state.AgentInfo{SessionID: sid, Transcript: tpath}}
			store := storeWith(t, sess)
			rs := newReconcileState(fanout.NewObserver(histDir), nil)
			sink, sinkDir := newTestSink(t)
			now := time.Now()

			return conformance.SamplerRun{
				Sample: func() { rs.sampleFanout(store.Snapshot()) },
				Detach: func() { renameAway(t, live, gone) },
				Apply: func() any {
					rs.observeFanout(sink, sess, sess.Claude, now)
					return struct {
						Events   []string
						InFlight int
					}{drainSink(t, sink, sinkDir), sess.Claude.InFlightSubagents}
				},
			}
		},
	})
}

// ---------------------------------------------------------------------------
// sampleSignals — the status self-heal's transcript tail
// ---------------------------------------------------------------------------

// The hottest reader the tick had: a stat plus a bounded tail read for every idle
// or working session, every tick, which during an active turn is every session.
//
// Its Detach is the one that cannot be a removal. selfHealStuckStatus re-stats the
// transcript under the lock (signalSample.freshFor) and discards a sample whose
// file has moved, so deleting the file would make the inline fallback fire
// LEGITIMATELY and the contract would be asserting the fallback rather than the
// sample. Rewriting the contents at the same size and mtime detaches the ANSWER
// while leaving the staleness stamp exactly where the sample left it.
func TestSamplerContract_sampleSignals(t *testing.T) {
	conformance.RunSamplerContract(t, conformance.Sampler{
		Name: "sampleSignals",
		Build: func(t *testing.T) conformance.SamplerRun {
			live, _ := liveAndGone(t)
			tpath := filepath.Join(live, "t.jsonl")
			since := time.Now().Add(-time.Minute)
			writeLines(t, tpath,
				`{"type":"user","timestamp":"`+since.Add(time.Second).UTC().Format(time.RFC3339Nano)+
					`","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`)
			// The stat gate compares mtime against StatusSince, so the file has to
			// look newer than the chip or nothing is read at all.
			touch := since.Add(2 * time.Second)
			if err := os.Chtimes(tpath, touch, touch); err != nil {
				t.Fatal(err)
			}

			sess := &state.Session{PID: 42, Agent: state.AgentKindClaude, CWD: "/proj",
				Claude: &state.AgentInfo{SessionID: "s42", Transcript: tpath,
					Status: state.StatusWorking, StatusSince: since}}
			store := storeWith(t, sess)
			sink, sinkDir := newTestSink(t)
			var signals map[int]signalSample

			return conformance.SamplerRun{
				Sample: func() { signals = sampleSignals(store.Snapshot(), testTune) },
				Detach: func() {
					fi, err := os.Stat(tpath)
					if err != nil {
						t.Fatal(err)
					}
					// Same size, same mtime, no signal in it.
					if err := os.WriteFile(tpath, bytes.Repeat([]byte("\n"), int(fi.Size())), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(tpath, fi.ModTime(), fi.ModTime()); err != nil {
						t.Fatal(err)
					}
				},
				Apply: func() any {
					m := map[int]*state.Session{sess.PID: sess}
					selfHealStuckStatus(m, time.Now(), testTune, sink, signals)
					return struct {
						Status string
						Events []string
					}{sess.Claude.Status, drainSink(t, sink, sinkDir)}
				},
			}
		},
	})
}

// ---------------------------------------------------------------------------
// sampleLabels — the Claude session name on disk
// ---------------------------------------------------------------------------

// A stat per session per tick, and a read plus an unmarshal whenever a `/name`
// lands. Cheap on a local disk and not cheap on a network $HOME, and nothing
// about resolving a name ever needed the lock — only recording the change does.
func TestSamplerContract_sampleLabels(t *testing.T) {
	conformance.RunSamplerContract(t, conformance.Sampler{
		Name: "sampleLabels",
		Build: func(t *testing.T) conformance.SamplerRun {
			live, gone := liveAndGone(t)
			// label.RawName resolves ~/.claude/sessions/<pid>.json off $HOME, so the
			// detachable tree IS the home directory here.
			t.Setenv("HOME", live)
			sessionsDir := filepath.Join(live, ".claude", "sessions")
			if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			const pid = 77
			writeLines(t, filepath.Join(sessionsDir, "77.json"), `{"name":"kettle"}`)

			sess := &state.Session{PID: pid, Agent: state.AgentKindClaude, CWD: "/proj",
				Claude: &state.AgentInfo{SessionID: "s77", Transcript: filepath.Join(live, "t.jsonl")}}
			store := storeWith(t, sess)
			rs := newReconcileState(nil, nil)
			sink, sinkDir := newTestSink(t)
			now := time.Now()
			var names map[int]string

			return conformance.SamplerRun{
				Sample: func() { names = rs.sampleLabels(store.Snapshot()) },
				Detach: func() { renameAway(t, live, gone) },
				Apply: func() any {
					rs.observeLabel(sink, names[sess.PID], sess, sess.Claude, now)
					return drainSink(t, sink, sinkDir)
				},
			}
		},
	})
}

// ---------------------------------------------------------------------------
// sampleMemory — /proc/<pid>/smaps_rollup for every session's whole tree
// ---------------------------------------------------------------------------

// The first reader to move out, and the one that set the rule: a full pass costs
// 10-20 ms because the kernel walks the target's mapping list to compute PSS, so
// it scales with the sessions being measured rather than with the machine.
func TestSamplerContract_sampleMemory(t *testing.T) {
	conformance.RunSamplerContract(t, conformance.Sampler{
		Name: "sampleMemory",
		Build: func(t *testing.T) conformance.SamplerRun {
			tree := testsupport.NewFakeProcTree(t)
			const pid, child = 100, 101
			tree.AddProcess(t, pid, testsupport.ProcSpec{Comm: "claude", PPid: 1,
				Rollup: &testsupport.RollupKB{Pss: 40 * kB, SwapPss: 1 * kB}})
			tree.AddProcess(t, child, testsupport.ProcSpec{Comm: "bash", PPid: pid,
				Rollup: &testsupport.RollupKB{Pss: 10 * kB}})
			tree.SetMemInfo(t, testsupport.MemInfo(3*1024*kB))

			sess := &state.Session{PID: pid, Agent: state.AgentKindClaude, CWD: "/proj",
				Claude: &state.AgentInfo{SessionID: "s100"}}
			store := storeWith(t, sess)
			rs := newReconcileState(nil, newMemorySamplerAt(tree.Root))
			sink, sinkDir := newTestSink(t)
			now := time.Now()
			var mem memoryTick

			return conformance.SamplerRun{
				Sample: func() { mem = rs.sampleMemory(store.Snapshot()) },
				// The processes exit. A tree the sampler can no longer read is the
				// honest shape of "gone" for /proc — there is nothing to rename.
				Detach: func() {
					tree.RemoveProcess(t, child)
					tree.RemoveProcess(t, pid)
				},
				Apply: func() any {
					// The real function reconcileOnce calls under the lock, not a copy
					// of its body: a copy would keep passing over its own stale lines the
					// day a read is added to the production one.
					mem.applyLocked(sess, sink, now)
					return struct {
						AgentBytes int64
						TreeBytes  int64
						Events     []string
					}{sess.MemAgentBytes, sess.MemTreeBytes, drainSink(t, sink, sinkDir)}
				},
			}
		},
	})
}

// ---------------------------------------------------------------------------
// sampleProc — liveness and job-control suspension, one process read each
// ---------------------------------------------------------------------------

// The liveness verdict is the sample the DURABLE session_end backstop runs on, and
// sweepDeadSessions is the first thing inside the tick's Apply.
//
// Detach here swaps the backend rather than removing a file, because this sampler
// reads through an osproc.Source. Note what that does and does not prove today:
// sweepDeadSessions takes only the verdict map, so it has no source to consult and
// the detached run cannot currently differ. The registration still earns its place
// twice over — the negative control is live (an unswept lane is a ghost lane), and
// the swap is already in position for the day the sweep grows a source parameter.
func TestSamplerContract_sampleProc(t *testing.T) {
	conformance.RunSamplerContract(t, conformance.Sampler{
		Name: "sampleProc",
		Build: func(t *testing.T) conformance.SamplerRun {
			const pid = 5100
			var src osproc.Source = fakeProcSource{st: map[int]procState{pid: procGone}}
			sess := &state.Session{PID: pid, Agent: state.AgentKindClaude, CWD: "/proj",
				Claude: &state.AgentInfo{SessionID: "s5100"}}
			store := storeWith(t, sess)
			sink, sinkDir := newTestSink(t)
			now := time.Now()
			var procs map[int]procSample

			return conformance.SamplerRun{
				Sample: func() { procs = sampleProc(store.Snapshot(), src) },
				// Every pid now reads as a live claude, so an under-lock re-read would
				// reach the opposite verdict from the sample.
				Detach: func() { src = fakeProcSource{} },
				Apply: func() any {
					var remaining int
					store.Apply(func(m map[int]*state.Session) {
						sweepDeadSessions(m, procs, sink, func(int) {}, now)
						remaining = len(m)
					})
					return struct {
						Events    []string
						Remaining int
					}{drainSink(t, sink, sinkDir), remaining}
				},
			}
		},
	})
}

// ---------------------------------------------------------------------------
// sampleUsage — the transcript delta behind usage_sample
// ---------------------------------------------------------------------------

// The odd one out, and registered precisely because it is: usage moved out
// WHOLESALE rather than being sampled-then-applied, since it mutates no session
// state and its sink is non-blocking. So its under-lock phase is empty, and what
// this pins is that the emptiness is real — every usage event exists before the
// lock is ever taken, and the tick's Apply adds nothing to them.
//
// That makes the equality assertion structurally certain and the NEGATIVE CONTROL
// the load-bearing half: it is what would catch usage drifting back inside the
// Apply, where it lived until it was the last transcript read left under the lock.
func TestSamplerContract_sampleUsage(t *testing.T) {
	conformance.RunSamplerContract(t, conformance.Sampler{
		Name: "sampleUsage",
		Build: func(t *testing.T) conformance.SamplerRun {
			live, gone := liveAndGone(t)
			tpath := filepath.Join(live, "t.jsonl")
			writeLines(t, tpath, `{"type":"system"}`)

			sess := &state.Session{PID: 7, Agent: state.AgentKindClaude, CWD: "/proj",
				Claude: &state.AgentInfo{SessionID: "s7", Transcript: tpath}}
			store := storeWith(t, sess)
			rs := newReconcileState(nil, nil)
			sink, sinkDir := newTestSink(t)
			now := time.Now()

			// First sight primes the cursor to EOF and emits nothing, so the tokens
			// below are the only thing any run can report.
			rs.sampleUsage(store.Snapshot(), nil, sink, now)
			f, err := os.OpenFile(tpath, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(assistantUsageModelLine("claude-opus-4-8", 100, 40) + "\n"); err != nil {
				t.Fatal(err)
			}
			f.Close()

			return conformance.SamplerRun{
				Sample: func() { rs.sampleUsage(store.Snapshot(), nil, sink, now) },
				Detach: func() { renameAway(t, live, gone) },
				// There is no under-lock phase. The answer is what the sample already
				// emitted, which is the whole claim.
				Apply: func() any { return drainSink(t, sink, sinkDir) },
			}
		},
	})
}
