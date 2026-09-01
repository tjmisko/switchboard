# `$mod+A` bounded attention ring

> **Goal.** `switchboard-ctl attention` moves within the urgent part of the
> session list without letting repeated presses climb from red all the way to
> green. `cycle next|prev` remains the unrestricted session navigator.
>
> Origin: [attention-latency-report.md](attention-latency-report.md) §2.4–§2.5.
> The first implementation used one ring containing all three colors. The
> bounded rule below supersedes that implementation after the live
> red/orange/green counterexample on 2026-08-31.

## 1. Rule

Partition navigable, actionable sessions by status, preserving snapshot order
inside each tier:

```text
permission = red
idle       = orange
working or delegating = green
```

Build the ring from the most urgent populated tier and **at most the adjacent
color above it**:

| Snapshot contains | `$mod+A` ring |
|---|---|
| any red | red + orange |
| no red, any orange | orange + green |
| only green | green |
| only grey / no navigable rows | empty |

The ceiling is derived from the whole snapshot, not from the currently focused
session. A press advances one member in this bounded ring, skips every session
whose window already reports focused, and wraps at the end. If focus is outside
the ring, the next press enters at its most urgent member.

Grey, headless, and unbound remote sessions never join the attention ring.
Grey and ordinary navigable sessions outside the urgency ceiling remain
reachable through `cycle next|prev` and `pick`.

## 2. Consequences

```text
Sessions: R(red), O(orange), G(green)       bounded ring = [R, O]

focused R -> O
focused O -> R
focused R -> O
...
```

Green is unreachable for as long as red remains. The orange tier is a place to
toggle to, not a staircase that unlocks green on the next press.

```text
Sessions: R(red), G(green)                  bounded ring = [R]

focused R -> no-op
focused G -> R
```

The empty adjacent orange tier is not skipped. This is the deliberate exception
to the old "always moves" rule: unrestricted movement belongs to `cycle`, while
`attention` enforces the urgency ceiling.

```text
Sessions: O1(orange), O2(orange), G(green)  bounded ring = [O1, O2, G]

focused O1 -> O2 -> G -> O1
```

With no red present, green is exactly one layer above the most urgent color and
is therefore reachable.

## 3. Implementation

The behavior is local to `cmd/switchboard-ctl/main.go`:

1. `attentionRing` partitions navigable rows into red, orange, and green.
2. It returns `red+orange` when red is populated, `orange+green` when only
   orange is populated, and `green` otherwise.
3. `nextAttentionTarget` advances within that derived ring and skips all
   focused members, which preserves the WezTerm split-window safety property.
4. No daemon state, RPC field, persisted schema, or compositor behavior changes.

## 4. Regression gates

`cmd/switchboard-ctl/main_test.go` pins these cases:

- repeated presses over one red, one orange, and one green alternate red/orange
  and never target green;
- red plus green with no orange cannot skip the empty adjacent layer;
- with no red, orange can advance to green;
- red and orange retain snapshot order within their tiers;
- delegating remains a first-class green member when green is inside the bound;
- grey, headless, unbound-remote, and already-focused rows are never selected;
- a lap visits every member of the **bounded** ring and returns to its start.

## 5. Definition of done

- With red, orange, and green all present, any number of `$mod+A` presses stays
  within red/orange.
- Removing the last red recomputes the next press's ring as orange/green; no
  navigation state is cached between presses.
- `go test ./cmd/switchboard-ctl` and `go test ./...` pass.
- Help text and README distinguish bounded `attention` from unrestricted
  `cycle`.
