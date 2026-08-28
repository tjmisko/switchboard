# #5 / #6 / #7 — Quantize at construction, let the change key inherit it

**PR 2, together with #8. Prereq: #4.5 landed and re-measured.**

All three tasks are the same move applied three times: **quantize a value where
it is constructed, and let the change key inherit the quantization** rather than
teaching the key about it. The owner already made that call for the title
(#5 option b).

**#6 without #7 is a correctness regression, which is why they ship together.**
If the key quantizes `fresh_until` but the wire carries the raw value, two frames
that differ in a real way get one key, the republish is suppressed, and the
consumer's `fresh_until` falls behind truth → false "stale". The ceiling is what
makes the suppression *sound*, not a cosmetic follow-up.

### The soundness argument (put it in the doc comment — it is what gets reviewed)

Let the client's last frame be published at `p` carrying ceiling `L`. No
republish since `p` means the key did not change, and the key encodes the same
ceilinged value the wire carries, so `ceil(F_now) = L`. If the daemon holds the
graph fresh at `now`, then `now < F_now ≤ L`. Wire `observed_at` is
`O_p ≤ O_now ≤ now`, so the client's `Fresh(now)` is true. ∎

Two corollaries:

- **`observed_at` may be dropped from the key** — it only moves forward, and a
  lagging wire value only makes `Fresh`'s lower bound earlier, never falsely
  stale.
- **`fresh_until` may NOT be dropped, only quantized.** The ablation's "drop
  `fresh_until` entirely → 2.27/s" row is unreachable by design; it breaks the
  proof. **Do not treat 2.27/s as a target.**

---

## Commit sequence

| | commit | contents |
|---|---|---|
| C1 | `feat(panetitle): share one spinner-stripping title normalizer` | new leaf package + `terminal`/`mapping`/`label` wired (**#5**) |
| C2 | `feat(state): publish the agent-graph freshness horizon on a 5s ceiling` | `FreshnessBucket`, `CeilFreshUntil`, `AgentGraph.MarshalJSON` (**#7**) |
| C3 | `feat(state): drop pure clock churn from the publish change key` | `SnapshotChangeKey` rewrite (**#6**) |
| C4 | `docs: record the ceiling and the newly-advisory wire fields` | `state-schema.md`, golden regen, plan corrections |

C2 before C3 is deliberate: C3 then reduces to two field exclusions, and there is
never a moment where the key quantizes something the wire does not.

---

## #5 — Shared title normalization

### New package `internal/panetitle`

A leaf package (imports only `strings` + `unicode/utf8`), matching the repo's
existing small-package idiom (`internal/projectname`, `internal/durfmt`,
`internal/panebind`).

```
internal/panetitle/panetitle.go
    const SpinnerGlyphs = "◐◑◒◓⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✳⠂⠐⠁⠈⠠⠄⡀⢀"   // promoted from mapping
    func Normalize(title string) string
```

The table is `internal/mapping.spinnerPrefixes` (`mapping.go:299`), verified a
strict superset of `internal/label.spinnerPrefixes` (`label.go:32`,
`"✳ ⠂ ⠐ ⠁ ⠈ ⠠ ⠄ ⡀ ⢀"`). So `label` **gains** coverage of the braille and circle
spinners it currently misses — a Claude session titled `"⠹ my-project"` now
labels as `my-project`.

Rejected alternative: putting `NormalizeTitle` on `internal/terminal`. It works
(mapping already imports terminal) but drags `os/exec` and `internal/wezterm`
into `internal/label`, which today has no process dependencies.

### Trap 1 — `PaneRef.Title` must stay RAW

**Normalize `WindowTitle` only.** `cmd/switchboard/main.go:1371` runs the H9
stuck-chip recovery through `titleShowsIdleGlyph(sess.Wezterm.Title, tun.IdleTitleGlyphs)`
(`main.go:1424-1431`), which reads the **first rune** of the pane's own title and
matches it against the configured idle glyphs. Stripping that rune silently
disables the H9 demotion rule — a chip strands green forever with nothing
failing.

`internal/terminal/wezterm.go:78` sets `Title`, `:79` sets `WindowTitle`. Touch
only `:79`, and `internal/terminal/tmux.go:106`. Those two are the **only**
`PaneRef` constructors in the tree, and both `Locate` and `Snapshot` funnel
through them — which is what makes the conformance fix work.
`internal/wezterm/wezterm.go:168` (the raw CLI driver) also stays raw; it is
below the seam.

**Write a regression test asserting `Title` still carries its glyph**, with a
comment naming H9 and `main.go:1371`.

### Trap 2 — `Normalize` must be a fixed point

`mapping.normalizeTitle` (`mapping.go:300`) strips **one** leading rune. Under
option (b) that is not idempotent, and idempotence is exactly what option (b)
makes load-bearing:

- `Normalize("⠋ ⠙ foo")` → `"⠙ foo"`, and `Normalize("⠙ foo")` → `"foo"`.
- Worse than a failing test: `matchUniqueClient` (`mapping.go:267-269`)
  normalizes the *already-normalized* pane title against the *raw* WM title. For
  a two-glyph title the pane side is one strip ahead and **a join that works
  today breaks**.

So `Normalize` strips leading spinner runes **repeatedly** (glyph, then
whitespace, then glyph again) until the first rune is not one, then trims
surrounding whitespace. Empty in → empty out. This is a strict superset of both
existing behaviours and cannot regress a realistic title.

### Call-site conversions

- `internal/mapping/mapping.go:295-311` — delete `spinnerPrefixes` and
  `normalizeTitle`; `matchUniqueClient` calls `panetitle.Normalize`.
- `internal/label/label.go:32` + `:87-95` — delete the `[]string` table and the
  `CutPrefix` loop; call `panetitle.Normalize`.

### The two raw-title consumers, checked rather than assumed

- `cmd/switchboard-ctl/main.go:154` prints `title=%q` in `list`. Reads **better**
  stripped — today a `list` snapshot shows whichever spinner frame was up. No
  change needed.
- `internal/rpc/rpc.go:1433-1434` builds `session=<id> "<title>"` for
  diagnostics. Same conclusion; its `strings.TrimSpace` becomes redundant but
  harmless. No change needed.

Note both were checked in the commit message.

### Acceptance

`internal/conformance` `TestWeztermLocatorConformance` / `TestAutoLocatorConformance`
green with Codex sessions spinning:

```sh
SWITCHBOARD_LIVE_CONFORMANCE=1 go test ./internal/conformance -count=5
```

The live assertions are **gated off by default**. Two notes:

- Minor correction to the plan text: `conformance.go:430` compares
  `conformance.Pane` (`Mux`, `PaneID`, `TTY`, `WindowTitle` —
  `conformance.go:293-298`), **not** the whole `terminal.PaneRef`. It does not
  carry `Title`, which is why leaving `Title` raw is safe there.
- **Residual risk:** this closes the disagreement only for titles differing by
  *leading* spinner glyphs. The 23/23 evidence over 5 runs is strong but the
  suite samples a live terminal. If a future Codex build churns the title *body*
  (`"⠹ Working… (12s · ↑1.2k)"`), the fix is **not** more normalization — it is
  dropping `WindowTitle` from `conformance.Pane`.

---

## #7 — The freshness ceiling, applied at the wire boundary

> **Owner decision, 2026-08-26: the in-memory policy change is DEFERRED.** The
> originally recommended design applied the ceiling in `ProjectAgentGraph`, which
> would make every provider's freshness horizon genuinely longer by up to
> `FreshnessBucket`. The owner is not comfortable with that yet. This section
> specifies the **wire-only** mechanism instead. The policy change stays
> documented in §"Deferred alternative" and can be revisited with measurements.

### Mechanism

Add to `internal/state/agent_graph.go`:

```go
// FreshnessBucket is the quantum a published freshness horizon is rounded UP to.
const FreshnessBucket = 5 * time.Second

// CeilFreshUntil rounds t up to the next FreshnessBucket boundary, returning t
// unchanged when it is already on one, or zero.
func CeilFreshUntil(t time.Time) time.Time

// MarshalJSON publishes fresh_until on its bucket ceiling so a consumer that
// received no frame while the daemon suppressed a within-bucket republish still
// evaluates Fresh correctly. In-memory semantics are untouched: the daemon's own
// Fresh() continues to use the true horizon.
func (g AgentGraph) MarshalJSON() ([]byte, error)
```

Implement `MarshalJSON` with the standard alias trick to avoid infinite
recursion:

```go
func (g AgentGraph) MarshalJSON() ([]byte, error) {
	type wire AgentGraph          // sheds the method set
	w := wire(g)
	w.FreshUntil = CeilFreshUntil(g.FreshUntil)
	return json.Marshal(w)
}
```

Because the method is on the **value** receiver, both `AgentGraph` and
`*AgentGraph` get it, so `Session.AgentGraph` (a pointer) is covered.

### Why this satisfies #6 for free

`SnapshotChangeKey` is a `json.Marshal` of the snapshot, so it goes through the
same `MarshalJSON` and inherits the ceiling. **#6 therefore needs no
`fresh_until` rule at all** — that is the whole point of quantizing at the
boundary, and the change-key doc comment must say so explicitly.

### Two implementation landmines — both need tests

1. **The ceiling must be a true fixed point.** `t.Truncate(d).Add(d)` advances an
   *already aligned* value by a whole bucket. Federation re-encodes remote frames
   and `state.json` is hydrated back in, so a non-idempotent ceiling would walk
   `fresh_until` forward one bucket per hop, forever. Return `t` unchanged when
   `t.Truncate(d)` equals `t`.
2. **Compare with `.Equal()`, not `==`.** `time.Time` `==` compares wall +
   monotonic + location. Snapshot times carry a monotonic reading; `Truncate`
   strips it, so `==` would never report "already aligned" and the function would
   never be idempotent.

Preserve the input's location — forcing UTC would change the encoded string for
every locally-stamped horizon for no benefit.

### The honest cost of choosing wire-only

**Cost 1 — encode overhead.** A nested `json.Marshal` per `AgentGraph` on every
snapshot encode: the change key, the broadcast, and the persist. The designer's
estimate is roughly a doubling of the `agent_graph` portion of each. Bounded and
acceptable — `SnapshotChangeKey` already marshals the whole snapshot on every
`Apply`, and after Phase 1 the publish rate is <0.5/s — but it is real overhead
added in a PR whose purpose is removing allocation churn. **Measure it**: compare
`publish-stats`' `heap_sys_mb` before and after C2.

**Cost 2 — a bounded daemon/consumer divergence.** The daemon greys at the true
horizon (`agentgraph.Reduce` sets `Runtime: unknown` at `reduce.go:11`) while the
consumer still considers the graph fresh up to the ceiling. For ≤5 s the chip can
be grey while the tooltip shows live rows with no ` · stale` marker. Cosmetic and
bounded, but it is the price of not changing in-memory policy, and it does not
exist under the deferred alternative.

**Cost 3 — the divergence is not uniform, which is the subtle one.** There is a
`MarshalJSON` but no `UnmarshalJSON`, so any graph that makes a round trip comes
back with the **ceiling as its in-memory value**:

- remote sessions decoded by `internal/remotestate`
- graphs hydrated from `state.json` at daemon start
  (`internal/state/agent_graph.go:277` `hydrateAgentGraph`)

Local, live graphs keep the raw horizon; round-tripped ones effectively get the
policy change. **Document this explicitly** — it is a genuine asymmetry, and a
reader who assumes "in-memory is always raw" will be wrong for two real paths.
Note also that `hydrateAgentGraph` deliberately sets
`observation.FreshUntil = now` to expire a restored Codex user-input latch
immediately. The in-memory summary still expires immediately; a subsequent
encode ceilings that already-grey graph's horizon by ≤5 s, which does not
restore its authority.

### The current staleness consumers

- **`cmd/claude-tui/main.go` is the only renderer that still calls
  `AgentGraph.Fresh(now)`.** Current Waybar replaced its per-agent tree with an
  event-driven fanout summary and has no stale marker to unify. Keep the minimal
  direct TUI predicate; extracting a one-caller helper would obscure the seam.
- **Leave `cmd/switchboard-ctl/diagnose.go:220-227` alone in this PR.** Its
  four-way `undated`/`not_yet_valid`/`expired`/`fresh` split is strictly finer
  than `Fresh`'s boolean and is its own output contract (pinned by
  `diagnose_test.go:163-172`). Add a comment and file a follow-up for an
  `AgentGraph.FreshnessClass()` both could share.
- **`agentgraph.Observation.Fresh` (`internal/agentgraph/types.go:163`) — no
  change.** It evaluates a provider-owned observation in-process against the
  horizon that provider stated, and `Observation` has no JSON tags and never
  reaches a consumer. Add a one-line comment so the next reader does not "fix"
  the asymmetry.

### `internal/history/agent_state.go:71` — unaffected

`AgentStateProjector.Project` receives the **raw `agentgraph.Observation`** from
the provider (`agent_observation.go:468`), never a `state.AgentGraph`. Under the
wire-only mechanism nothing about history timing changes at all — which is one
more argument for this choice over the deferred alternative, where the
`expireCurrent` transition row would have been stamped ≤5 s later.

### Deferred alternative — the in-memory lease

Apply `CeilFreshUntil` in `ProjectAgentGraph` (`internal/state/agent_graph.go:115`)
instead of at encode time. Every `state.AgentGraph` the store holds passes
through that funnel, so there is **one representation** — memory, wire,
`state.json`, change key, federation — and no divergence of any kind, no encode
overhead, and `Fresh`/`Clone` untouched.

The cost is that it is a **real policy change**: every provider's horizon grows
by ≤ `FreshnessBucket`, which moves `expireCurrent`'s grey-out, the diagnose
`expired` classification, and the `transition` row `expireCurrent` emits
(stamped ≤5 s later with a longer `dur_prev_ms`).

Also rejected during design, for the record: projecting the ceiling in
`snapshotLocked` (mirroring `StatusSinceWire`). `Store.Snapshot()` is **not** a
wire-only path — `rpc.streamLocalSnapshots` re-reads it per edge and
`federation.View.Snapshot()` reads it too, as do daemon-internal readers
(`agent_observation.go:457/551-552/626-627`,
`codex_hook_transitions.go:353/437/711`). You would get the identical
behavioural surface as the policy change *plus* a memory-vs-wire divergence.

---

## #6 — The change key

`SnapshotChangeKey` is exported from `internal/state/state.go`; #8 reuses it for
the aggregate View rather than inventing another comparator.

### Honour the doc comment's argument

Its case for encode-over-comparator is that a comparator's failure mode is
silent: add a field to `Session`, forget to compare it, bars stop updating with
nothing failing. **Keep that property.** The rewrite stays a full `json.Marshal`
of the snapshot with specific named fields *neutralized on a copy*, never a
hand-written field list:

```
func SnapshotChangeKey(snap Snapshot) []byte:
    sessions := shallow copy of snap.Sessions
    for each session with a non-nil AgentGraph:
        g := *session.AgentGraph            // value copy
        g.ObservedAt = time.Time{}
        g.Nodes = fresh copy of the node slice
        for each node: node.UpdatedAt = time.Time{}
        session.AgentGraph = &g
    marshal the same anonymous struct as today over `sessions`
    on error: log + return nil   (unchanged: fail open)
```

Both fields carry `omitzero`, so zeroing removes them from the key bytes
entirely. A newly added field with a JSON tag still lands in the key by
construction.

**`FreshUntil` and `WindowTitle` get no rule here** — they arrive already
quantized from C2 and C1. Say so explicitly in the comment, with pointers to
`CeilFreshUntil` and `panetitle.Normalize`.

### ⚠ The dangerous bug available here: aliasing

`Apply` (`state.go:540-547`) passes the **same `snap`** to
`adoptPublishedLocked` and then to `broadcast` and `persist`, and
`Session.AgentGraph` is a pointer shared with that outgoing frame. Zeroing
`snap.Sessions[i].AgentGraph.ObservedAt` in place would blank `observed_at` and
every node's `updated_at` **on the wire and in `state.json`** — a silent schema
regression the golden cannot catch (it has no `agent_graph` block).

Hence the value copy **and** the fresh `Nodes` slice, and hence a mandatory test:
`should leave the frame it keys untouched`.

Cost: one `[]Session` copy plus one `[]AgentNode` copy per graph-bearing session
per `Apply`, under the write lock — trivial against the 18 KB encode already
happening there.

### Why node `updated_at` is dropped, not bucketed

- For Codex it is a **poll stamp, not a transition stamp** — it moves in 73/230
  pairs with no sibling field on that node moving (verified for 72 of 73). The
  tooltip's `· 0s` today means "time since the last poll", which is meaningless.
  Dropping it makes the anchor "time since the last publish-worthy change",
  which is what the tooltip claims to show.
- It re-enters the key indirectly the moment the node's
  `runtime`/`attention`/`lifecycle`/`usage`/`completed_at` moves — exactly when a
  publish is warranted.
- `started_at` and `completed_at` **stay** in the key. Those *are* transition
  times.

Its only consumer is `barlayout.AgentStateAt`
(`internal/barlayout/agenttree.go:208-216`), a cosmetic age anchor.

### The doc comment needs an honest restatement

Today it claims equal keys are "indistinguishable both on the subscribe stream
and in `state.json`". After C3 that is **no longer literally true**:
`observed_at` and node `updated_at` are on the wire but out of the key. Rewrite
around what a consumer can *act on*, naming the exclusion classes:

1. `UpdatedAt` — restamped every snapshot; the reason a naive comparison never
   fires. (unchanged)
2. `json:"-"` fields — excluded by construction. (unchanged)
3. **New:** `AgentGraph.ObservedAt` and `AgentGraph.Nodes[i].UpdatedAt` —
   advertised but pure clock; their wire values may lag. Cite the soundness proof
   for `observed_at`, and the cosmetic-anchor argument for node `updated_at`.
4. **New:** the derived-value rule — `window_title` and `fresh_until` need no
   rule because they are quantized at construction. Do not add one.

### Bucket width

`state.FreshnessBucket` is one named constant, and every test derives its offsets
from it so a width change re-tunes the suite automatically. **Do not tune it
against the pre-#4.5 ablation.** Its doc comment must record the coupling the
ablation table cannot see:

> This width bounds how long a republish can be suppressed, and therefore how far
> a consumer's `fresh_until` may lead the daemon's true horizon. The shortest
> provider horizon is `codex.DefaultFreshness = 15 s`
> (`internal/provider/codex/observer.go:19`). Raising this above roughly a third
> of that widens the daemon/consumer divergence materially; revisit `Fresh`'s
> consumers before doing so.

There is a genuine tension with the ablation, which shows 30 s buying more
suppression than 5 s. **Post-#4.5 that trade should evaporate** (residual
0.15/s), so the realistic post-measurement direction is "5 s is fine, possibly
narrower". If it does not evaporate, that is the signal to reopen the deferred
in-memory lease, which decouples width from divergence entirely.

### The seam for #8

Export, and tell whoever implements #8:

- `state.SnapshotChangeKey(snap Snapshot) []byte` — rename the unexported func;
  update `adoptPublishedLocked` (`state.go:594`) and `golden_test.go:216`.
- `state.FreshnessBucket`, `state.CeilFreshUntil`
- `panetitle.Normalize`, `panetitle.SpinnerGlyphs`

**Advice for #8:** `federation.View` merges frames from a *different process*
whose build version you do not control. A remote on an older build sends raw
titles and unceilinged `fresh_until`, and the shared key would churn on them. The
right fix is not a defensive key — it is applying `panetitle.Normalize` and
`state.CeilFreshUntil` at federation's **remote-decode boundary**, the same
option-(b) principle one seam over. Both are idempotent, so it is free where the
remote is current. `View.Snapshot()` copies whole `state.Session` values, so
local rows inherit everything with no work.

---

## Tests

### `internal/panetitle/panetitle_test.go` (new)

- `TestNormalize` — table driven **over the runes of `SpinnerGlyphs` itself**, so
  the table cannot fall behind the constant. Cases: each glyph + space + name;
  glyph with no space; empty; whitespace only; glyph only; name starting with a
  normal letter (untouched); a glyph in the *interior* (untouched); a
  non-spinner leading emoji (untouched).
- `TestNormalizeIsIdempotent` — for every case above **plus a multi-glyph
  prefix** (`"⠋ ⠙ foo"`), which is the case a strip-once implementation fails.
- `TestSpinnerGlyphsCoversTheLegacyLabelTable` — pins the superset claim so the
  two tables cannot silently diverge back apart.

### `internal/terminal`

- `TestWeztermPaneRefShouldStripTheSpinnerFromTheWindowTitle` — two
  `wezterm.Pane` values differing only in the leading glyph produce **identical**
  `PaneRef`s.
- `TestWeztermPaneRefShouldLeaveThePaneOwnTitleRaw` — asserts `PaneRef.Title`
  still carries its glyph, with a comment naming H9 and `main.go:1371`. **This is
  the guard against the worst available mistake in #5.**
- The same pair for `tmux.go`'s `WindowName`.
- Existing `TestWeztermPaneRefShouldCarryTheMuxIdentityAndDecodedCWD` keeps a
  spinner-free title and stays green unchanged.

### `internal/mapping`

- Existing `TestMatchUniqueClientNormalizesActivitySpinners`
  (`mapping_test.go:83-99`) stays green as-is.
- Add `TestMatchUniqueClientJoinsAnAlreadyNormalizedPaneTitleToASpinningWMTitle` —
  pane title stripped (the post-#5 production shape), WM title carrying any
  glyph → still joins. This is the idempotence claim **at the join**, and it is
  what the fixed-point change protects.

### `internal/label`

Add a braille case to the existing `RawName` window-title fallback tests
(`label_test.go:59-72`) proving the newly gained coverage; existing `✳` cases
unchanged.

### `internal/state/agent_graph_test.go`

- `TestCeilFreshUntilRoundsUpAndIsIdempotent` — zero stays zero; an
  already-aligned instant returns **unchanged** (via `.Equal`, and confirm it
  holds for a value carrying a monotonic reading); a mid-bucket instant rounds
  up; `CeilFreshUntil(CeilFreshUntil(t))` equals `CeilFreshUntil(t)`; the result
  is never before the input.
- `TestAgentGraphMarshalJSONPublishesTheCeiling` — a graph with a mid-bucket
  horizon encodes to the ceiling.
- `TestAgentGraphMarshalJSONLeavesTheInMemoryHorizonUntouched` — **the guard on
  the deferred policy decision.** After marshalling, `g.FreshUntil` and
  `g.Fresh(now)` are unchanged.
- `TestAgentGraphMarshalJSONRoundTripIsStable` — marshal → unmarshal → marshal
  produces identical bytes (the fixed-point requirement, at the boundary that
  actually round-trips).

### `internal/state/broadcast_test.go` — extend, do not duplicate

Existing suite: `:131` `publishesNothingWhenNothingChanged`, `:181`
`republishesWhenTheLastPersistFailed`, `:220` `publishesWhenAnObservableFieldChanges`,
`:269` `publishesWhenStatusSinceMoves`, `:297` `publishesWhenOnlyInMemoryFieldsChange`,
`:333` `publishesWhenCapabilitiesChange`. `requireQuiet` (`:57-64`) is the
no-publish helper — broadcast is synchronous inside `Apply`, so **no sleeps
anywhere**.

New, all on a fixed clock `time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)`:

- `TestApply_shouldNotRepublishWhenOnlyTheTitleSpinnerRotates` — seed with
  `panetitle.Normalize("⠋ proj")`, re-apply with `panetitle.Normalize("⠙ proj")`,
  `requireQuiet`. Test-only import of `panetitle` from `package state_test`.
  Naming the normalizer in the test is what makes it fail if either the fixed
  point or the gate regresses. (A test writing raw spinners into the store would
  assert behaviour that, per option (b), deliberately does not exist — that claim
  lives in the terminal-package test.)
- `TestApply_shouldNotRepublishWhenOnlyObservedAtAdvancesWithinABucket` — two
  projections whose `observed_at` advances 1 s with identical nodes and a horizon
  inside one bucket; `requireQuiet` **and** byte-compare `state.json`
  before/after.
- `TestApply_shouldRepublishWhenFreshUntilCrossesABucketBoundary` — advance the
  horizon past `FreshnessBucket`; assert a broadcast, and assert the broadcast's
  `fresh_until` is the new ceiling.
- `TestApply_shouldRepublishWhenANodeRuntimeStateChanges` — a node's `runtime`
  moves while its `updated_at` is untouched; must publish. Mirror:
  `updated_at` alone moves → `requireQuiet`.
- `TestApply_shouldRepublishWhenASessionsLabelOrStatusChanges` — extend the
  existing `publishesWhenAnObservableFieldChanges` table (`:222-229`) with a
  `DisplayName.Value` case and an `AgentGraph.Summary.Status` case rather than
  writing a new test.
- `TestSnapshotChangeKeyShouldLeaveTheFrameItKeysUntouched` — compute the key,
  then assert the snapshot's `observed_at` and every node's `updated_at` are
  unchanged **and** that the broadcast frame carries them. **The aliasing guard.**
- `TestPublishedCeilingCoversEverySuppressedInstant` — the property test behind
  the proof: apply a run of observations advancing `observed_at` across one
  bucket, assert exactly one publish, and assert the published `fresh_until` is
  strictly after every true horizon that was suppressed.

### `internal/state/golden_test.go`

`TestChangeKeyIgnoresClocksOnly` extends the "must not move the key" table with
`observed_at` advancing and a node `updated_at` advancing, and keep every
existing "must move the key" row. This becomes the pure-function complement to
the `Apply`-level tests, built from the golden's own bytes so a newly added wire
field is covered the moment the fixture is regenerated.

### `cmd/claude-tui/main_test.go`

`TestShouldNotMarkTheTUIStaleWhenTheDaemonSuppressedAWithinBucketRepublish` —
the DoD test. Build a one-child graph and call `renderSnapshot` with its explicit
`now`; no clock injection is needed. Fixed clock
`time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)`.

- Build the graph as a consumer receives it: observed at `t`, provider horizon
  `t+1s`, `FreshUntil: state.CeilFreshUntil(t.Add(time.Second))`.
- Assert `renderSnapshot` at
  `t.Add(state.FreshnessBucket - 100*time.Millisecond)`
  does **not** contain `" · stale"`, and does contain an age counter.
- Assert at `t.Add(state.FreshnessBucket + 100*time.Millisecond)` that stale is
  **allowed** — a one-sided assertion, exactly as the DoD words it.
- Deriving both offsets from `state.FreshnessBucket` is what makes the constant
  trivially changeable.

### `cmd/switchboard`

- `publish_suppression_invariant_test.go` must stay green **unmodified** through
  all four commits. If it needs editing, stop and re-read why it exists.
- Under the wire-only mechanism there is no `expireCurrent` timing shift, so the
  history-side test the lease design required is not needed. **Verify that** by
  running `./internal/history/` rather than assuming it.

### Existing tests to check

Under the wire-only mechanism the two breakages the lease design predicted
(`agent_observation_test.go:403` and `:422-449`) **should not occur**, because
in-memory `FreshUntil` is untouched. Run `go test ./...` and confirm; if either
goes red, the ceiling is leaking into memory somewhere and that is a bug in the
`MarshalJSON` implementation, not a test to adjust.

---

## Docs (C4)

`docs/state-schema.md`:

| line | edit |
|---|---|
| `:31-36` | the "`updated_at` is advisory" paragraph now covers three fields; generalize and forward-reference |
| `:139` | `window_title` — "The pane's **spinner-stripped** window title. The daemon strips leading agent activity glyphs and surrounding whitespace at the terminal seam; the raw title is not preserved anywhere on the wire. Still a best-effort join key to the WM window title, which is **not** stripped — normalize both sides before comparing." |
| `:332` | `observed_at` — excluded from the publish gate, so on a quiet session it is the observation time as of the last publish-worthy change. Its only contractual role is `Fresh`'s lower bound; never a liveness signal |
| `:333` | `fresh_until` — **invalidated sentence, rewrite.** "Exclusive end of the authority interval, **rounded up to a 5 s ceiling on publication**. `observed_at <= now < fresh_until` still defines freshness. The daemon suppresses republishes while only `observed_at` moves inside the bucket, so a consumer that has received no new frame can still evaluate `Fresh` correctly. The daemon's own internal horizon is the unrounded value, so the published ceiling may lead it by up to one bucket." |
| `:341-342` | the "should still use the explicit freshness timestamps" reinforcement — add that those timestamps carry the suppression guarantee |
| `:348` | `summary.runtime` — say measured against the daemon's unrounded horizon, which is why a chip can grey up to a bucket before a consumer would call the graph stale |
| `:382` | node `updated_at` — advisory, excluded from the publish gate; display age only, never a liveness or ordering key |
| `:460-461, :490-491, :520-521, :550-551` | example `fresh_until` values are `:25`, `:25`, `:15`, `:25` — **all already 5 s-aligned, no edit needed**; add a note that examples show the published ceiling and must stay aligned if the width changes |
| `:621` | example `window_title` already spinner-free ✓ |

`docs/timing-hazards.md` (H9): one line noting `WeztermInfo.Title` is
deliberately **not** spinner-stripped while `WindowTitle` is, and that H9 depends
on it.

`docs/memory-footprint-plan.md`: see `05-corrections-to-the-plan.md`.

---

## The golden fixture

**Implemented in C4.** Neither #5 nor #7 changed any pre-existing golden line:
`canonicalSnapshot()` is hand-built, its `window_title` was already
spinner-free, and its prior timestamps were aligned. The fixture did, however,
omit the largest optional wire block, `agent_graph`, despite its stated rule that
every optional field be present somewhere.

C4 adds a populated root-plus-child `agent_graph`, including every usage field,
on a **30 s** grid (`09:04:00Z` / `09:04:30Z`). The fixture therefore stays
green for any bucket width dividing 30 s and the constant remains genuinely
re-tunable.

How to be sure the diff is only what you intended:

1. Regenerate **only after** C1–C3: `UPDATE_GOLDEN=1 go test ./internal/state`.
2. `git diff --stat` must show exactly one file changed.
3. `git diff internal/state/testdata/state.golden.json` must show **only
   additions** — the new `agent_graph` object. Zero deletions, zero modified
   lines. **If any pre-existing line moved, stop**: something else changed on the
   wire, and the aliasing bug is the first thing to check (it shows up as
   `observed_at`/`updated_at` vanishing).
4. Re-run without `UPDATE_GOLDEN` and confirm all four golden tests pass.

---

## What to re-measure, and when

- **Before C1** — nothing new; the post-#4.5 GATE 1 numbers are the control.
- **After C1 (#5)** — the acceptance run
  (`SWITCHBOARD_LIVE_CONFORMANCE=1 go test ./internal/conformance -count=5`),
  5/5 green before proceeding. Also one `sb-mem-baseline` capture: #5 alone
  should move the rate materially, and a capture that does not move is a signal
  the normalizer is not reaching the store.
- **After C2 (#7)** — compare `publish-stats`' `heap_sys_mb` against C1 to
  quantify the `MarshalJSON` overhead. This is the number that decides whether
  the deferred in-memory lease is worth reopening.
- **After C3 (#6+#7 complete)** — ≥3 `sb-mem-baseline` captures under comparable
  load, cross-checked against `publish-stats`. Expected landing zone with #4.5
  in: ~0.15/s.
- **Throughout** — `publish_suppression_invariant_test.go` green, and a day-file
  spot check (`agent_state` row counts per root, before vs after).

---

## Real uncertainties

1. **The wire-only mechanism buys a divergence and an encode cost** that the
   deferred lease does not. Both are bounded and measurable; C2's `heap_sys_mb`
   comparison is the evidence that decides whether to revisit.
2. **The round-trip asymmetry** (cost 3 above) means "in-memory is always raw" is
   false for remote and hydrated graphs. This is inherent to `MarshalJSON`
   without `UnmarshalJSON` and must be documented rather than fixed — adding
   `UnmarshalJSON` to strip the ceiling is not possible, since the raw value is
   not recoverable from the ceiling.
3. **The fixed-point `Normalize` is a behaviour change to
   `mapping.normalizeTitle`**, not a pure refactor. Realistic titles never carry
   two leading glyphs, so the blast radius should be nil — but the multi-glyph
   mapping-join test is what proves it is the safe direction.
4. **The conformance acceptance is empirical, not guaranteed** — see #5's
   residual risk.
5. **Dropping `observed_at` from the key weakens the doc comment's stated
   contract.** The soundness proof covers the only consumer that acts on it, but
   the contract sentence must be rewritten rather than left standing.
6. **Cross-timezone key churn is pre-existing and not fixed here.** Two producers
   stamping the same instant in different locations encode to different strings
   and so to different keys. `CeilFreshUntil` preserves location, so it neither
   creates nor cures this. It is #4.5's to collapse; if it survives #4.5, that is
   the moment to consider forcing UTC on graph timestamps — a separate, reviewed
   wire change.
