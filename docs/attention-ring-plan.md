# `$mod+A` bounded attention ring

> **Goal.** `switchboard-ctl attention` jumps to the most urgent available
> session. When already there, it can toggle at most one layer upward without
> letting repeated presses climb from red to green. `cycle next|prev` remains
> the unrestricted session navigator.
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
session. Selection is urgency-first rather than a simple walk around that ring:

1. If focus is outside the most urgent populated tier, jump to that tier's first
   member.
2. If focus is already in the most urgent tier, select another un-focused member
   of the same tier in snapshot order.
3. Only when no other urgent member is available may the press toggle to the
   adjacent tier.

Every candidate already reported focused is skipped. This matters for multiple
panes that share one focused WeZTerm window.

Grey, headless, and unbound remote sessions never join the attention ring.
Grey and ordinary navigable sessions outside the urgency ceiling remain
reachable through `cycle next|prev` and `pick`.

## 2. Consequences

```text
Sessions: R(red), O1(orange), O2(orange), G(green)
bounded ring = [R, O1, O2]

focused R  -> O1
focused O1 -> R
focused O2 -> R
```

Green is unreachable for as long as red remains. Crucially, focus on either
orange returns directly to red; orange siblings are not traversed before the
urgent jump. The orange tier is a one-layer toggle, not a second navigation
ring or a staircase that unlocks green.

```text
Sessions: R1(red), R2(red), O(orange)       bounded ring = [R1, R2, O]

focused R1 -> R2
focused R2 -> R1
focused O  -> R1
```

An available equal-urgency peer takes precedence over relaxing to orange.

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

focused O1 -> O2
focused O2 -> O1
focused G  -> O1
```

With no red present, orange becomes the most urgent tier. Green is reachable as
the one-layer toggle only when a single orange is already focused; any press
from green returns to orange.

## 3. Implementation

The behavior is local to `cmd/switchboard-ctl/main.go`:

1. `attentionRing` partitions navigable rows into red, orange, and green.
2. It returns `red+orange` when red is populated, `orange+green` when only
   orange is populated, and `green` otherwise.
3. `nextAttentionTarget` first searches the most urgent tier. It considers the
   adjacent tier only when every urgent member is already focused, preserving
   the WezTerm split-window safety property.
4. No daemon state, RPC field, persisted schema, or compositor behavior changes.

## 4. Regression gates

`cmd/switchboard-ctl/main_test.go` pins these cases:

- repeated presses over one red, one orange, and one green alternate red/orange
  and never target green;
- either of two focused oranges jumps directly to the red rather than to its
  orange sibling;
- multiple reds cycle among themselves before relaxing to orange;
- red plus green with no orange cannot skip the empty adjacent layer;
- with no red, green jumps to orange;
- red and orange retain snapshot order within their tiers;
- delegating remains a first-class green member when green is inside the bound;
- grey, headless, unbound-remote, and already-focused rows are never selected;
- a singleton urgent session can toggle one layer up and the next press returns
  to urgency.

## 5. Definition of done

- With red, orange, and green all present, any number of `$mod+A` presses stays
  within red/orange.
- With any red present, a press from any orange or green targets red.
- Removing the last red recomputes the next press's ring as orange/green; no
  navigation state is cached between presses.
- `go test ./cmd/switchboard-ctl` and `go test ./...` pass.
- Help text and README distinguish bounded `attention` from unrestricted
  `cycle`.
