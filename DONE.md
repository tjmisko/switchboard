# DONE — merged milestones

A running log of completed milestones (newest last). See
`docs/portability-plan.md` for the full phase plan and per-task DoD.

## Phase 0 — Behavioral regression baseline (test-first gate)

The project shipped zero tests; Phase 0 built a behavioral suite pinning current
observable behavior so the portability refactor is provably regression-free.
`go test -race ./...` is green; the suite asserts neutral observables (not OS
mechanism) so it carries forward to the macOS backend.

- **0.0** Froze the `state.json` public contract — `docs/state-schema.md` +
  `testdata/state.golden.json` round-trip test (`UPDATE_GOLDEN=1` regen).
- **0.1** `docs/behavior-spec.md` — full per-function "should … when …" backlog,
  traceability matrix to 0.3–0.8, register of the 12 ⚠ items.
- **0.2** `internal/testsupport/` harness (golden, fs, ScriptedConn/LineReader,
  wezterm runtime-dir, fake `/proc` tree + `ProcStatus`, linux pty
  `SpawnTTYChild`, real-child `SpawnSleep`); zero `internal/*` imports; a seed
  test in every package.
- **0.3** Pure unit tables; extracted the pure `bottombar.shouldRun` core.
- **0.4** `state` in-memory tests under `-race`; extracted
  `hyprland.parseEvents(io.Reader)`; wezterm/runtimeDir cases.
- **0.5** Behavior-preserving testability seams — `discovery.Scanner` procSource,
  `mapping.matchUniqueClient`, `rpc.findTrackedAncestor(readProc)`; verified
  identical with an isolated daemon smoke-run.
- **0.6** `procwatch` death-semantics against real children (once / dup no-op /
  Stop-no-fire / ESRCH-immediate).
- **0.7** `internal/conformance/` — exported, backend-agnostic
  `RunSourceContract`/`RunManagerContract`/`RunLocatorContract`, reused verbatim
  in Phases 2–4. Verified green against the live Linux/Hyprland/wezterm adapters
  (`SWITCHBOARD_LIVE_CONFORMANCE=1`); the ⚠ 0x-normalization round-trip asserts
  purely in CI.
- **0.8** `bottombar` `bottomBarOps` seam + stub-driven F8 / no-flap /
  idempotence tests. Process-group kill, reaper, self-heal left integration-level
  (Hyprland-only extra).
- **0.9** CI matrix (`go vet`/`build`/`test -race` on linux amd64+arm64, darwin
  build-only stub); `docs/decisions.md` verdicts for all 12 ⚠ items;
  seam-critical fix landed — deterministic `Snapshot` order via PID tie-break.

**Next:** Phase 1 — interface extraction (`internal/osproc`, `internal/terminal`,
`internal/wm`) + `none` backends. Phase 1 works through `docs/decisions.md`
seam-by-seam, flipping each deferred ⚠ as it extracts that seam.
