# Seed-Replay Memory Incident — remediation plan

> **Goal.** Eliminate the multi-gigabyte transient allocation the fanout
> Observer's first-sight seeding performs (two full-store `ReadRange`
> materializations per session), which OOM-killed the daemon three times on
> 2026-08-26 and turned a systemd restart into a kill loop. Four cuts, in
> order, each independently landable and each re-measured with the same
> apparatus:
>
> 1. **Measurement apparatus** — a seed benchmark that exercises the real
>    seeding code path, plus permanent in-daemon seed telemetry. Lands FIRST,
>    before any behavior change, so before/after numbers come from identical
>    measurement code.
> 2. **Remove memory sampling** — the `memory_sample` event stream (Aug 3,
>    63dc1a5) is 67% of store bytes / ~80% of recent lines and was judged
>    scope creep. Feature deleted; a scrub script rewrites existing day-files.
> 3. **Streaming shared-pass seeding** — replace per-session double
>    materialization with one streaming fold over the log that seeds every
>    session at once. Peak heap goes from O(store) to O(result sets).
> 4. **Seed cursor** — a derived snapshot (per-file byte offsets + the folded
>    sets) written once per daemon start after seeding, so the next restart
>    streams only appended tail bytes. Silently rebuilt from full replay on
>    any validation failure. The log stays the sole source of truth.
>
> **Design stance** (agreed 2026-08-26): accurate agent history is a product
> feature. No windowed replay — a bounded window would drop spans for
> subagents that ran entirely during a daemon outage older than the window.
> The cursor dominates windowing: bounded work AND zero span loss. Seeding
> stays synchronous — the ordering constraint (no emission before seed) makes
> async machinery cost more than making the work small.

---

## 1. The incident (root cause, established 2026-08-26)

### 1.1 Mechanism

- The Observer's spawn source is a dir scan
  (`transcript.SubagentsForTranscript`, `internal/fanout/observer.go`) that
  re-sights every historical `agent-<id>.meta.json` on every reconcile tick;
  metas and workflow run dirs are never deleted. Emission is deduped by
  in-memory seen-sets that die with the process.
- On first sight of a session (`observer.go` seeding block), the daemon
  rebuilds those seen-sets by calling `history.PriorSubagentState` **and**
  `history.PriorWorkflowState` (`internal/history/reader.go`), each of which
  runs `ReadRange(dir, zero, zero)` — decoding **every line of every
  day-file** into one `[]Event`, then filtering by session ID.
- The store grew ~100x after `memory_sample` landed (63dc1a5, Aug 3): day
  files went from ~1MB/day to 15–42MB/day. Retention never bit
  (`retain_days=90` reaches nothing until late Sept; `max_bytes=1GB` unhit).
- A daemon restart un-seeds every live session at once, so the first
  reconcile after restart runs 2 full-store scans × N live sessions — which
  is why the OOM kill became a kill *loop*.

### 1.2 Timeline (journal, boot -1 and boot 0, 2026-08-26)

| When | Event |
|---|---|
| 14:27:35 | codex session discovered — seeding spike absorbed (survived) |
| 14:30:51 | claude session discovered — seeding begins |
| 14:31:13 | global OOM; daemon killed at 2.56GB anon (13h old, unit peak 3G + 1.2G swap, 29 discoveries that day) |
| 14:31:18 | systemd restart #1: all ~5 live sessions re-seed → 3.4G peak in 48s wall / 27.1s CPU → OOM-killed |
| 14:32:06 | restart #2 → 3.5G peak in ~20s / 18.1s CPU → user shutdown + reboot |
| 14:39:45 (boot 0) | fresh daemon; one session discovered 14:40:08 → `MemoryPeak=4.05GB`, then RSS settles at ~50MB; steady state ~288MB |

Corroboration: `switchboard-ctl` (same `ReadRange` path) was OOM-killed on
**Aug 6** — two days after sampling fattened the store. No switchboard OOM
kills exist in the journal before Aug 6.

---

## 2. The bad state, quantified (baseline inputs)

Measured 2026-08-26 on goosebook (Asahi, 8 cores, 15.2GB RAM, 16K pages),
installed production build.

**Store** (`~/.local/state/switchboard/history`):

| Metric | Value |
|---|---|
| Total jsonl bytes | ~459MB (du -sb dir incl. backups: 478,889,740) |
| Total events | 859,836 lines across 55 day-files |
| `memory_sample` bytes | 323,616,068 (67% of bytes, ~80% of recent lines) |
| Daily volume pre/post 63dc1a5 | ~1MB/day → 15–42MB/day |

**Unit cost of ONE full-store materialized read**
(`/usr/bin/time -v switchboard-ctl timeline -since 2026-06-24 -until
2026-08-26 -json`):

| Metric | Value |
|---|---|
| Peak RSS | **4,969,456 KB (~4.97GB)** — 10.8x inflation of input |
| Wall | 7.78s |
| CPU | 10.5s (135%) |

Seeding runs **two** of these per session, serialized under `o.mu`.

**Fixture**: the incident-reproducing store is frozen at
`~/.local/state/switchboard/bench-fixture-20260826` (out of git — it is
personal activity data). All benchmark runs point at the fixture (or a
scrubbed copy of it), never the live store.

---

## 3. Measurement apparatus (phase 1 — lands before any fix)

### 3.1 `switchboard-ctl seed-bench` (hidden subcommand)

- Flags: `-dir` (store), `-sessions id1,id2,…` (explicit IDs) or `-storm`
  (seed every session ID found in the log — the restart case).
- Calls **the same functions the daemon's seeding calls** — before the
  refactor that is `PriorSubagentState` + `PriorWorkflowState` per session;
  after it, the shared streaming seed. Never a reimplementation: the numbers
  must move when the code moves.
- Emits one JSON line per pass: `{sessions, events_scanned, events_matched,
  bytes_read, wall_ms, cpu_ms, heap_alloc_peak, total_alloc, num_gc, vm_hwm_kb}`
  (`runtime.MemStats` + `VmHWM` from `/proc/self/status`).

### 3.2 `scripts/sb-bench-seed` (wrapper, sibling of `sb-perf`)

- Runs `seed-bench` in a **fresh subprocess per scenario** (VmHWM is a
  process-lifetime high-water mark), 3 runs each, prints the table.
- Scenarios: `single` (1 session — the discovery case) and `storm` (all
  sessions — the restart case).
- Carries the frozen baseline in its docstring, `sb-perf`-style, with the
  "re-measure under comparable host conditions" caveat.

### 3.3 In-daemon seed telemetry (permanent)

- One log line per seed pass in the existing idiom:
  `fanout-seed: sessions=N events=… matched=… bytes=… wall=… cpu=…
  heap_peak=…`, plus a `VmHWM` line once initial seeding completes.
- This is the durable production before/after evidence — the same journal
  the RCA was built from — and a standing regression tripwire: a future
  store-shape regression shows up as a fat `fanout-seed` line, not an OOM.

### 3.4 Containment guards (not the fix; regression insurance)

- `systemd/switchboard.service`: `MemoryHigh=1G`, `MemoryMax=2G`,
  `Environment=GOMEMLIMIT=512MiB`. Keep `systemd/service_test.go` in sync.
- **These land AFTER the streaming cut, never with the apparatus**: a 2G
  `MemoryMax` on the current algorithm (which needs ~5GB to seed) would turn
  every daemon restart into a guaranteed kill loop. Deploy order matters.

---

## 4. The cuts

### Phase 2 — remove memory sampling + scrub

- Delete the sampler (c500274) and `memory_sample` emission (63dc1a5), the
  `Config.Memory` field and its `max_bytes` defaulting split
  (`defaultMemoryMaxBytes` goes away; 100MB default is right again at
  post-sampling volume), and the `switchboard-ctl memory` surface.
- Old configs with a `memory` key parse fine (unknown JSON keys ignored).
  The user's explicit `max_bytes=1GB` remains their choice.
- `scripts/scrub-memory-samples`: rewrites each day-file without
  `memory_sample` lines (JSON-parse the type — no substring false
  positives), temp-file + rename, originals preserved under
  `.scrub-backup-<ts>/` (precedent: `.repair-backup-20260814`). Run with the
  daemon stopped (today's file is append-live). Expected: store ~459MB →
  ~135MB.

### Phase 3 — streaming shared-pass seeding

- New `history.SeedScan(dir) (SeedIndex, error)`: one `bufio.Scanner` pass,
  one line in memory at a time; a `bytes.Contains` prefilter on the four
  event types (`subagent_spawn/stop`, `workflow_start/stop`) skips ~all
  lines before any `json.Unmarshal`; matches fold directly into per-session
  set maps. No `[]Event` is ever materialized.
- The Observer builds the index lazily on first seed request (under the
  existing `o.mu` seeding path), keeps the snapshot for the process
  lifetime, and serves every later first-sight from it. Staleness is a
  non-issue: the daemon is the log's only writer and its own in-memory
  state covers everything after startup.
- `PriorSubagentState`/`PriorWorkflowState` are deleted (their tests move to
  `SeedScan`). `ReadRange`/`ReadDay` remain for ctl surfaces.

### Phase 4 — seed cursor

- File: `<history-dir>/.seed-cursor-v1.json` — `{version, files: {day →
  bytes_consumed}, sessions: {id → {spawned, stopped, wf_started,
  wf_stopped}}}`.
- **Written once per daemon start**, immediately after the initial
  `SeedScan` completes (offsets = file sizes at scan time). No runtime
  flushing, no writer-goroutine coupling: everything the daemon appends
  during its life is recovered on the next start by streaming the tail past
  the recorded offsets.
- Load-time validation: any recorded file missing or smaller than its
  offset → the cursor is discarded and a full `SeedScan` rebuilds it,
  **silently** (agreed). New files not in the cursor are read from 0.
  A scrub/repair that rewrites day-files therefore auto-invalidates.
- Conformance test: cursor-seeded sets ≡ full-replay sets on the same store
  (property checked on the test fixtures; run manually against the frozen
  bench fixture during capture).

---

## 5. Capture protocol

1. Land phase 1 (apparatus only). Capture **baseline** on the fixture:
   `single` and `storm`, 3 runs each. Expected to reproduce ~5GB VmHWM
   single-pass; storm several GB more (or death).
2. Land each phase; after each, re-run the identical protocol. Report the
   scrub's contribution separately (same algorithm, scrubbed fixture copy)
   from the code's (new algorithm, same fixture).
3. Once per phase, restart the real daemon and keep the journal
   `fanout-seed` lines as the production record.
4. Freeze final numbers in `scripts/sb-bench-seed`'s docstring and in §6.

## 6. Success criteria / results

| Metric | Bad state | Target | Result |
|---|---|---|---|
| Single-seed peak (VmHWM over baseline) | ~5GB | <50MB | _tbd_ |
| Storm seed (all sessions) | 3.4GB in 48s, then OOM | ≈ single (shared pass) | _tbd_ |
| Cold full rebuild wall | ~8s/pass × 2N passes | <2s total (scrubbed store) | _tbd_ |
| Warm cursor seed | n/a (didn't exist) | <50ms | _tbd_ |
| Daemon `MemoryPeak` over a day | 3–4GB | <400MB | _tbd_ |
| Span accuracy | exact (by replay) | exact (conformance-tested) | _tbd_ |

## 7. Follow-ups (out of scope here)

- `switchboard-ctl timeline`/ctl `ReadRange` surfaces still materialize the
  full requested range (a ctl was OOM-killed Aug 6). Post-scrub this is
  ~1.5GB peak for a full-history query — survivable but ugly; a streaming
  timeline fold is a separate cut.
- Deploy steps for the live machine: stop daemon → run scrub on the real
  store → install new binaries → start daemon (first start pays one cheap
  full SeedScan, writes the first cursor).
