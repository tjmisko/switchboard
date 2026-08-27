# #9 — Separate rate limit for `windowtitlev2`

**PR 3, on its own. Prereq: #6 complete.** With the change key normalized, the
remaining cost of a title event is the fork pair, which is what this task bounds.

Least subtle of the six tasks, but it has a wider blast radius than it looks.

---

## Goal

`drainWMEvents` coalesces title-only layout events in a ~1.5 s window while
`openwindow` / `movewindowv2` keep the 200 ms `layoutDebounce`. A title event
must never delay a pending open/move flush.

**Measured acceptance:** `wezterm cli list` forks ≤ 1/s with two spinning Codex
panes, over a 15 s window.

---

## The blocking fact: `wm.Event` cannot distinguish the raw events today

`internal/wm/wm.go:34-52`:

```go
const (
	// EventFocusChanged: the active window changed; Address is the new active
	// window (Clients form), "" if nothing is focused.
	EventFocusChanged EventKind = "focus-changed"
	// EventWindowClosed: a window closed; Address is the closed window.
	EventWindowClosed EventKind = "window-closed"
	// EventLayoutChanged: a window moved / retitled / opened — something that may
	// change a session's mapping. Address is unset; the daemon re-reconciles.
	EventLayoutChanged EventKind = "layout-changed"
)

// Event is a neutralized window event. Address is in Clients() form (already
// normalized by the backend), or "" for EventLayoutChanged.
type Event struct {
	Kind    EventKind
	Address string
}
```

`Event` carries only `Kind` + `Address`, and `Address` is deliberately left empty
for layout events (`internal/wm/hyprland.go:73-75`). **So the DoD's "extend
`wm.Event` or split `EventLayoutChanged` into `EventTitleChanged`" is required,
not optional.**

### The mapping, and the second source of truth

`internal/wm/hyprland.go:86-94`:

```go
// rawEventKinds maps the raw socket2 event names the backend recognizes to the
// neutral kind they translate to.
var rawEventKinds = map[string]EventKind{
	"closewindow":    EventWindowClosed,
	"activewindowv2": EventFocusChanged,
	"movewindowv2":   EventLayoutChanged,
	"windowtitlev2":  EventLayoutChanged,
	"openwindow":     EventLayoutChanged,
}
```

**`internal/wm/hyprland.go:114-116` `CanonicalEvents()` returns the same five
names as a hand-maintained slice** — a second source of truth, contract-tested by
`internal/conformance/conformance.go:206-216` via `TranslateEvent`. Keep them in
sync or the conformance suite will tell you, loudly.

**Other backends map the same way and need the same split:**
`internal/wm/i3.go:158-163` (`"title": EventLayoutChanged` at `:162`) and
`internal/wm/x11.go:194-195`.

### Where the raw events are parsed

Two layers; the mapping is **not** in `internal/hyprland`:

- `internal/hyprland/hyprland.go:165-168` — the raw wire type, `Event{Name, Data}`
  split on `">>"`.
- `internal/hyprland/hyprland.go:199` — `parseEvents(ctx, r io.Reader, ch chan<- Event)`,
  the extracted-for-testability reader (doc at `:194-198`). It is **name-agnostic**
  — it forwards every delimited line; filtering happens one layer up.
- `internal/wm/hyprland.go:59-82` — `Subscribe` translates, dropping unknown names
  at `:68-71`.

---

## `drainWMEvents` — read the existing semantics before changing them

**It is at `cmd/switchboard/main.go:591`, not `:580`** (the plan's citation is
stale).

The hazard comment the DoD requires updating, `cmd/switchboard/main.go:545-562`:

```go
// layoutDebounce is the window a burst of layout events is coalesced into.
// Three raw Hyprland events map to EventLayoutChanged — movewindowv2,
// windowtitlev2, openwindow (internal/wm/hyprland.go) — and each one re-resolves
// EVERY session, which costs a full terminal + WM enumeration. windowtitlev2 is
// the hazard: a running agent CLI repaints its pane title as it works, so an
// uncoalesced stream multiplies that enumeration by the title rate.
//
// Rate limit, NOT a plain trailing-edge debounce: the timer is armed by the
// first event of a burst and not re-armed by the rest, so it always fires within
// one window of the burst starting. A re-arming debounce would have no maximum
// wait at all — under events spaced closer than the window it never fires — and
// the staleness bound would quietly become the reconcile interval. See
// drainWMEvents.
//
// So the cost really is at most one window of staleness on a chip's workspace,
// and the burst's last state still lands: an event arriving after a firing finds
// the timer disarmed and arms it again.
const layoutDebounce = 200 * time.Millisecond
```

**Preserve the rate-limit property in both windows.** A re-arming debounce has no
maximum wait under a dense stream — which is exactly what a spinning Codex pane
produces.

Body anchors:

| line | what |
|---|---|
| `:595-598` | a single `timer := time.NewTimer(debounce)`, `timer.Stop()`, `pending := false` |
| `:601-608` | `flush := func()` → `timer.Stop(); pending = false; reresolveAll(...)` |
| `:622` | `if evt.Kind != wm.EventLayoutChanged {` … `:638` `handleWMEvent(...)`; flush-before-dispatch at `:637` |
| `:651-653` | arm-once: `if !pending { timer.Reset(debounce); pending = true }` |
| `:655-657` | `case <-timer.C: pending = false; reresolveAll(...)` |

There is **exactly one** timer and one `pending` bool. Two windows means a second
timer/flag pair **and a way to know which kind armed which**.

`reresolveAll` (doc `:723-729`, signature `:730`) calls `resolver.Enumerate(ctx)`
inside `turn.Do` at `:737` — that is the single fork pair per firing
(`wezterm cli list` + `hyprctl clients`). Its comment ends: *"layoutDebounce
exists to ration it."* The dead-in-production `case wm.EventLayoutChanged` inside
`handleWMEvent` is at `:711-719`.

Call site: `runWMLoop` at `:564`, passing the constant at `:576`.

---

## The injection seam exists; a fake clock does not

The doc comment at `main.go:589-590` says so verbatim:

```go
// debounce is a parameter rather than the layoutDebounce constant so tests can
// drive the coalescing without sleeping a real 200ms per case.
```

**But there is no injected clock anywhere.** All six tests in
`cmd/switchboard/wm_debounce_test.go` use real `time.Sleep` with
`testDebounce = 40 * time.Millisecond` (`:17`) — see `:99`, `:111`, `:113`,
`:131-137`, `:150`, `:183`, `:188`, `:214-219`. A grep for
`Clock`/`nowFunc`/`timeNow` across `cmd/switchboard/` and `internal/wm/` returns
only prose about the wall clock.

So the DoD's "no real sleeps — use the injected debounce" means one of:

- **(a)** extend the existing shrunken-duration pattern to **two** injected
  durations (still wall-clock, but small and matching house style), or
- **(b)** build a clock seam that does not exist today.

**Pick deliberately and say which in the commit message.** Do not silently ship
wall-clock sleeps under a DoD that forbids them. (a) is the smaller change and is
consistent with the six existing tests; (b) is the honest reading of the DoD.
Recommend (a) plus a note in the plan that the DoD's wording was aspirational,
unless the owner wants the clock seam built.

### Reusable harness

`cmd/switchboard/wm_debounce_test.go`:

| helper | line | what |
|---|---|---|
| `testDebounce` | `:17` | `40 * time.Millisecond` |
| `countingLocator` | `:23-44` | counts per-session re-resolves via `Locate` |
| `stubManager` | `:50-59` | a `wm.Manager` stub |
| `debounceHarness(t)` | `:63-89` | returns `(*state.Store, chan wm.Event, *countingLocator, stop func())`; the goroutine call is at `:83` |

Existing tests: `:91` coalesce burst → 1; `:106` two separated bursts → 2; `:122`
focus not queued behind layout; `:142` flush on stream close; `:161` still fires
under a never-ending burst; `:200` pending layout lands before focus dispatch.

Also relevant: `cmd/switchboard/reconcile_lock_test.go:173` and `:284` call
`reresolveAll` directly.

---

## Required tests (from the DoD)

- `should coalesce a burst of title events into one re-resolve per window`
- `should still re-resolve a new window within the short window while titles are bursting`
- `should re-arm after a firing`

The middle one is the important one: it is the whole point of splitting the
windows, and it is what a naive "just use one longer window" implementation
fails.

Add, beyond the DoD:

- **A test that the two timers are independent** — a title event arriving while
  an open/move flush is pending must not extend or cancel it.
- **A backend-parity test** that the split reaches `i3` and `x11`, or an explicit
  note in the commit that it does not and why.
- **A `CanonicalEvents()` / `rawEventKinds` consistency check** if one does not
  already fall out of the conformance contract test.

---

## Order of work

1. `refactor(wm): distinguish title changes from other layout changes` — split
   the kind (or extend `Event`), update `rawEventKinds`, `CanonicalEvents()`,
   `i3.go`, `x11.go`, and the conformance contract. No daemon behaviour change
   yet: `drainWMEvents` treats the new kind exactly like `EventLayoutChanged`.
   Green before proceeding.
2. `feat(switchboard): give window-title events their own rate limit` — the second
   timer/flag pair in `drainWMEvents`, the updated hazard comment describing both
   windows, and the tests.
3. **Measure**: `strace -f -e trace=execve -p <daemon pid>` for 15 s with two
   spinning Codex panes; count `wezterm` execs. Want ≤ 1/s.

```sh
strace -f -e trace=execve -p $(systemctl --user show switchboard -p MainPID --value) 2>&1 \
  | grep -c wezterm
```

---

## Note on ordering within Phase 1

#9 is last because after #6 the change key no longer republishes on a title
spinner, so the *only* remaining cost of a title event is the fork pair. Doing #9
first would still bound the forks, but you would be measuring against a moving
baseline. If the fork rate turns out to matter for CPU before #6 lands, that is a
reason to reorder — but say so and re-measure rather than assuming.
