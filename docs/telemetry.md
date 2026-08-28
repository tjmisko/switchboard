# Daemon telemetry lines

The daemon writes a small number of **permanent** structured lines to its
journal. They are not debug output and not bench artifacts: each one exists
because a real incident could not be diagnosed from outside the process, and
each one is a standing tripwire for that incident recurring.

```sh
journalctl --user -u switchboard -g 'publish-stats|fanout-seed' --since -1h
```

Both lines share a house style: a `name:` prefix, then `key=value` fields in a
fixed order, no content (no titles, no paths, no session ids) — they are safe to
paste into an issue. Field order is part of the contract; a test pins it.

Conventions used below:

- `*_mb` figures are **process-wide**. `runtime.ReadMemStats` and `VmHWM` cannot
  be scoped to a subsystem, so on a busy daemon they carry everything else in
  flight too. Read them as "what did the PROCESS look like at this moment",
  never as "what did this subsystem allocate".
- `heap_alloc_mb` is live heap, `heap_sys_mb` is what the runtime has reserved
  from the OS (live heap + GC headroom + spans/stacks). RSS tracks
  `heap_sys_mb`, not `heap_alloc_mb`; a large gap between them is GC headroom,
  which is what `GOGC` tunes.
- `vm_hwm_mb` is `VmHWM` from `/proc/self/status`: the kernel's **peak** RSS for
  the life of the process. It never falls. A restart resets it, and so does
  systemd's `MemoryPeak`.

---

## `publish-stats`

**Emitted:** once a minute, for the life of the daemon, from
`state.Store.LogPublishStats` (`internal/state/publishstats.go`), started by
`cmd/switchboard/main.go`.

**Why it exists:** the 2026-08-26 footprint investigation found the daemon's
cost was not retained state — the live heap is 5–10 MB — but *churn*: 9.4
publishes/s of a 13.7 KB snapshot to 11 subscribers, every one of them provoked
by a braille spinner glyph rotating inside a pane title. Nothing inside the
process could report that; it took a 15 s socket capture and a frame differ.
This line makes the same measurement standing and free.

```
publish-stats: publishes=304 suppressed=63 store_subscribers=1 frame_bytes=1857 heap_alloc_mb=1 heap_sys_mb=10 vm_hwm_mb=14 window=1m0s
```

(A real line, from a daemon started fresh with four agent sessions already
running — the first window is dominated by discovery.)

| Field | Meaning | What a bad value means |
|---|---|---|
| `publishes` | `Store.Apply` calls in the window whose change key **differed** from the last published one, so they broadcast to subscribers and rewrote `state.json`. | High is the whole problem. Sustained > 1/min on an idle machine means something is mutating a field that reaches the change key on a clock rather than on a real event. **No baseline has been measured for this counter yet** — the ~7-11.5/s figures in `docs/memory-footprint-plan.md` §1.4 are the `subscribe-all` WIRE rate, and on a `-remote` daemon those frames come from `federation.View`, which has no change gate at all (`internal/federation/view.go:320`). They therefore over-count `Store.Apply` publishes by an unknown margin — 19 of 231 frames in the 16:45 capture were byte-identical to their predecessor and cannot have come from the Store gate. Take the first real reading from this line, not from the wire. Task #10's < 0.5/s DoD is stated against the wire rate; convert before comparing. |
| `suppressed` | `Apply` calls whose change key **matched**, so the publish was dropped. The mutation still ran, and history still recorded — suppression gates `broadcast` and `persist` only. | `suppressed=0` with a non-trivial `publishes` means the change gate is not firing at all: either every Apply really is a change (a churn source), or the key is picking up a field that always differs. `suppressed ≫ publishes` is the healthy shape for a reconciler that runs every 5 s. |
| `store_subscribers` | `len(Store.subscribers)` **at sample time** — an instantaneous count of in-process Store subscribers, not socket clients. | `federation.View` subscribes unconditionally, so the normal floor is `1`; `0` means the View is not running. Waybar and `bottombar watch` subscribe to the View over RPC and therefore do **not** increase this number. Use `sb-mem-baseline`'s `socket_connections` for client fan-out and Phase 2 verification. A Store count that climbs without bound is a leaked internal subscription. |
| `frame_bytes` | Mean size in bytes of the Store frames actually encoded in the window. | The denominator is frames encoded, not `publishes`: `broadcast` returns before the encode when no Store subscriber exists. Growth here is the local snapshot getting fatter. Do not multiply it by `store_subscribers` to estimate socket traffic; federation builds a different aggregate frame. Use `sb-mem-baseline` for the wire measurement. |
| `heap_alloc_mb` | `runtime.MemStats.HeapAlloc >> 20` at sample time. | Should sit at 5–10 MB. A monotone climb across successive lines is a retained-state leak; sawtooth is normal GC. |
| `heap_sys_mb` | `runtime.MemStats.HeapSys >> 20` at sample time. | This is what RSS follows. `heap_sys_mb` ≫ `heap_alloc_mb` with a high `publishes` is allocation churn keeping the heap goal inflated — the case `GOGC` addresses, and the reason the fix is to publish less rather than to tune GC. |
| `vm_hwm_mb` | Peak RSS for the process so far. | Compare against `systemctl --user show switchboard -p MemoryPeak`. A `vm_hwm_mb` far above the steady `heap_sys_mb` says the daemon had one bad moment (a seed pass, a burst) rather than a standing problem — look for the `fanout-seed` line near it. |
| `window` | Wall time the counts cover, measured (previous sample → this sample), not assumed from the ticker. | Always `1m0s` in production. The first line after a restart is longer, because the window opens when the `Store` is constructed so the startup burst is counted somewhere. Anything much longer than `1m0s` afterwards means the daemon was descheduled or stalled — and it is the field that keeps `publishes` honest, since a 5-minute window would otherwise report five minutes of publishes as if they were one. |

**Reset, not cumulative.** Each sample returns the window's counts and zeroes the
accumulator, so a line always reads as "this is what the last minute did".
There is no running total to difference, nothing to overflow on a daemon that
runs for months, and a sampler that dies loses one window instead of silently
reporting every publish since boot as one minute's worth.

**One-encode skew.** A publish is counted under `Store.Apply`'s write lock; its
frame is counted in `broadcast`, which runs after that lock is released. A
publish decided at 59.9 s can therefore have its frame counted in the next
window. The skew is bounded by one encode and self-corrects; it is why `frames`
is carried as its own denominator rather than assuming one frame per publish.

**A quiet daemon still logs.** A window with no activity emits
`publishes=0 suppressed=0 …`. Silence on this line must mean the daemon is not
running, never that it is idle.

---

## `fanout-seed`

**Emitted:** once per seed-index build, from
`fanout.Observer.ensureSeedIndexLocked` (`internal/fanout/seedtelemetry.go`).
In practice that is once per daemon start, on first need.

**Why it exists:** it is the journal the 2026-08-26 OOM forensics were
reconstructed from (`docs/seed-replay-memory-plan.md` §3.3). A future
store-shape regression should surface as a fat `fanout-seed` line long before it
surfaces as an OOM kill.

```
fanout-seed: source=cursor sessions=79 files=1 lines=149 matched=0 mb=0 wall=7ms cpu=5ms heap_alloc_mb=5 heap_sys_mb=19 vm_hwm_mb=12 err=-
```

| Field | Meaning | What a bad value means |
|---|---|---|
| `source` | `cursor` when the persisted seed cursor validated and only the tail was replayed; `scan` when it did not and every day-file was re-read. | `scan` on every start means the cursor is never validating — check `err=`, and check whether something is rewriting history files behind the daemon. One `scan` after an upgrade or a cursor-format change is expected. |
| `sessions` | Entries in the resulting seed index (`len(res.Index)`). | Should track the number of sessions with fanout history in the retention window. A number that grows without bound across days is retention not being applied. |
| `files` | Day-files opened. | `1` for a healthy `source=cursor` start (today's file only). A `cursor` start reading many files means the cursor was far behind. |
| `lines` | Lines streamed past. | With `source=scan` this is the whole retained history; it is the figure that used to reach millions and cost gigabytes. |
| `matched` | Lines that decoded to one of the four seeded event types. | `matched=0` on a `cursor` start is normal (nothing relevant in the tail). `matched=0` on a `scan` start with a large `lines` means the seeded event types are not present — a schema drift. |
| `mb` | Bytes streamed, in MB (`Stats.Bytes >> 20`). | Pair with `wall`/`cpu` to see the cost of the pass. This is the number `MemoryMax` used to be blown by. |
| `wall` / `cpu` | Wall time and process CPU (user+system, via `getrusage`) for the pass. Process-wide CPU, so a busy daemon inflates it; the difference still brackets the pass usefully. | Single-digit ms for `cursor`, low hundreds of ms for `scan`. Seconds means the replay is back to reading everything. |
| `heap_alloc_mb`, `heap_sys_mb`, `vm_hwm_mb` | As above, sampled right after the pass. | `vm_hwm_mb` here is the number the OOM story was told in. A `scan` that pushes it well past the steady-state figure is the regression this line watches for. |
| `err` | `-` when the load and the cursor write both succeeded, otherwise the error text. | A cursor **write** failure only costs the next start one full scan, so it is logged and otherwise ignored. A load failure keeps whatever was folded before the failure — a partial index re-emits at most what a first-ever run would. |

---

## Adding a line

Keep to the shape: `name: key=value …`, fixed field order, no user content, a
comment above the `log.Printf` saying which incident it answers, a row in the
table here, and a test that pins the format. A telemetry line whose window or
field set varies per deployment is one whose numbers cannot be compared, which
is why `publishStatsInterval` is a constant rather than a flag.
