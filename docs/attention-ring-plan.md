# `$mod+A` as a ring — implementation plan

> **Goal.** `switchboard-ctl attention` always moves rightward and never
> re-focuses the window the key was pressed in. It pops out of the current
> attention level exactly when there is nothing below the focused session
> *within that level*, and it is a no-op only when there is genuinely nowhere
> else to go.
>
> Diagnosis this comes from: [attention-latency-report.md](attention-latency-report.md) §2.4 (D1),
> §2.5 (D5).

---

## 1. The rule

Order every **navigable, actionable** session into one ring:

```
ring = [ …permission… , …idle… , …working/delegating… ]
         ↑ tier-major, snapshot order (oldest first) within each tier
```

A press advances **one step clockwise**, skipping any session that is already
focused, and wrapping past the end back to the first `permission`.

That single rule is exactly the requested behavior, because "pop out of the
current level" and "there is nothing below me in this level" are the same
statement about a tier-major ring:

| Focused session | Next in ring | Reads as |
|---|---|---|
| red #1 of 3 | red #2 | move right, still in the red level |
| red #3 of 3 | first orange | nothing below me in the red level → **pop out** |
| the only red | first orange | nothing below me in the red level → **pop out** |
| the only red, nothing else | *nil* | nothing below, nothing beyond → **no-op** |
| last green | first red | lap complete; the ring wraps |
| a grey session / a browser window | first red | not in the ring → jump to the most urgent |

### 1.1 Worked traces

```
Sessions:  R(red)  O1(idle)  O2(idle)  G1(working)          ring = [R, O1, O2, G1]

focused R    → O1     pop out of the singleton red level
focused O1   → O2     move right within the idle level
focused O2   → G1     nothing below in idle → pop out
focused G1   → R      wrap; the red nags once per lap
```

```
Sessions:  R1(red)  R2(red)  O1(idle)                        ring = [R1, R2, O1]

focused R1   → R2     move right within the red level
focused R2   → O1     nothing below in red → pop out
focused O1   → R1     wrap
```

```
Sessions:  R(red)  G1(working)  X(unknown)                   ring = [R, G1]

focused R    → G1     grey is not a ring member and no longer suppresses anything
focused G1   → R
focused X    → R      focused outside the ring → most urgent member
```

```
Sessions:  R(red) only                                        ring = [R]

focused R    → nil    every ring member is already focused → no-op
```

### 1.2 The trade this accepts

Today a press from *anywhere* jumps straight to the top tier, so a green session
with a red elsewhere reaches the red in one press. Under the ring it reaches the
next green instead, and the red at the end of the lap. **This is deliberate** and
is the price of "always move right": strict priority re-entry and a
non-ping-ponging pop-out are mutually exclusive while the red is still red.

- The urgency loss is bounded by one lap (≤ N presses, N = navigable sessions).
- The red still holds its colour in the bar, the tooltip and the TUI. Nothing
  about the *signal* is weakened — only the order in which the key visits it.
- If the lap turns out to feel too long in practice, the escape hatch is not to
  restore priority re-entry (that reintroduces the ping-pong) but to land
  [R4/R5 of the latency plan](attention-latency-report.md#3-recommended-next-steps)
  so a stale red stops existing in the first place.

---

## 2. What changes

All of it is in `cmd/switchboard-ctl/main.go`. No daemon change, no RPC change,
no state-schema change, and no new state anywhere — the ring is derived from the
snapshot on every press.

### 2.1 Replace three functions with two

| Today | Fate |
|---|---|
| `topAttentionTier(sessions) []*state.Session` (`:580`) | → `attentionRing`, returns the concatenated ring rather than one tier |
| `pickAttentionExact(sessions, focused) *state.Session` (`:525`) | → `nextAttentionTarget`, drops the `focused` parameter |
| `pickAttention(sessions, focusedPID) *state.Session` (`:553`) | **delete** — no non-test caller; the table test moves onto `nextAttentionTarget` |
| `sameExactSession(a, b)` (`:538`) | **delete** — the ring reads `Focused` directly and never compares identities |
| `sessionStatus(session) string` (`:606`) | keep unchanged |
| `sessionNavigable(session) bool` (`:474`) | keep unchanged |

### 2.2 `attentionRing`

```go
// attentionRing orders every navigable session into the ring `attention` walks:
// permission (red) first, then idle (orange), then working and delegating
// (green), each in snapshot order. Unknown (grey) sessions are excluded — they
// are not actionable, and `cycle next|prev` already reaches every session
// regardless of colour. Headless and unbound-remote rows are excluded by
// sessionNavigable, exactly as before.
func attentionRing(sessions []state.Session) []*state.Session {
	var permission, idle, working []*state.Session
	for i := range sessions {
		if !sessionNavigable(sessions[i]) {
			continue
		}
		switch sessionStatus(sessions[i]) {
		case state.StatusPermission:
			permission = append(permission, &sessions[i])
		case state.StatusIdle:
			idle = append(idle, &sessions[i])
		case state.StatusWorking, state.StatusDelegating:
			working = append(working, &sessions[i])
		}
	}
	ring := make([]*state.Session, 0, len(permission)+len(idle)+len(working))
	ring = append(ring, permission...)
	ring = append(ring, idle...)
	ring = append(ring, working...)
	return ring
}
```

Build into a fresh slice rather than `append(append(permission, idle…)…)`, which
would write into `permission`'s backing array.

### 2.3 `nextAttentionTarget`

```go
// nextAttentionTarget returns the session `attention` should focus: one step
// clockwise around the ring from the focused session, skipping every session
// that is ALREADY focused so the key always moves.
//
// Skipping (rather than merely "advancing past") is what makes the wezterm split
// case safe. Focused is a WINDOW flag today, so two sessions sharing one window
// both report it; skipping the whole focused set lands the press on a genuinely
// different window instead of on a sibling pane, which would look like another
// dead key. See docs/attention-latency-report.md §2.5 / R2.
//
// Returns nil only when the ring is empty, or when every ring member is already
// focused — the "nothing below, nothing beyond" terminal case.
func nextAttentionTarget(sessions []state.Session) *state.Session {
	ring := attentionRing(sessions)
	if len(ring) == 0 {
		return nil
	}
	start := 0
	for i, session := range ring {
		if session.Focused {
			start = i + 1
			break
		}
	}
	for step := 0; step < len(ring); step++ {
		if candidate := ring[(start+step)%len(ring)]; !candidate.Focused {
			return candidate
		}
	}
	return nil
}
```

Three behaviors fall out of the two loops and are worth reading off explicitly:

- **No session focused** (a browser window is active) → `start = 0` → `ring[0]`,
  the most urgent member. Unchanged from today.
- **Focused session is grey or non-navigable** → no ring member reports
  `Focused` → `start = 0` → `ring[0]`. Also unchanged, and it is what makes a
  grey Codex root stop stranding the key.
- **Focused session is in the ring** → `start` is its successor, and the scan
  walks forward past any sibling pane of the same window.

### 2.4 `cmdAttention`

```go
func cmdAttention(c *rpc.Client) {
	target := nextAttentionTarget(mustList(c).Sessions)
	if target == nil {
		return
	}
	focusSession(c, *target)
}
```

The whole "find the focused session first" preamble (`:508`–`:521`) goes away —
`nextAttentionTarget` reads `Focused` off the ring itself.

---

## 3. Behavior changes to pin as tests

Five observable changes. Each gets a row so a future refactor cannot quietly
undo it.

| # | Before | After | Why |
|---|---|---|---|
| **C1** | Singleton top tier + focused → re-focuses itself | advances to the next tier | the reported bug |
| **C2** | Last member of a multi-member tier → wraps to that tier's first member | advances to the next tier | "nothing below me in this level" |
| **C3** | Any grey session suppresses the green tier entirely → no-op | grey is excluded from the ring and suppresses nothing | the silent no-op (state 3) |
| **C4** | A `delegating` session counts toward `considered` but joins no tier, so it also suppresses the green tier | `delegating` joins the green tier as a first-class member | latent bug found while reading `topAttentionTier`; see §3.1 |
| **C5** | A press from a green with a red present jumps to the red | advances within green; reaches the red at the end of the lap | the accepted trade, §1.2 |

### 3.1 On C4

`topAttentionTier` increments `considered` for every navigable session but only
files `permission` / `idle` / `working` into a tier. `delegating` — the green
"idle main thread, teammates in flight" status, on the wire since the Phase-B
work — matches no case, so it inflates the denominator and makes
`len(working) == considered` false. A machine running two green sessions and one
delegating session therefore has **no attention target at all**, and `$mod+A`
does nothing. This is a second, independent source of the reported dead key, and
it disappears for free under the ring.

### 3.2 The test table

Rewrite `TestPickAttention` in `cmd/switchboard-ctl/main_test.go:120` as
`TestNextAttentionTarget`. The existing `sess(pid, status)` and
`headlessSess(pid, status, focused)` helpers stay; add a
`focusedSess(pid, status)` helper so `Focused` is expressible in the table.

Rows to keep, with unchanged expectations:

- should jump to the first permission session when several are waiting *(no focus)*
- should prefer permission over idle even when idle comes first
- should jump to the only idle session when no permission exists
- should cycle to the next permission session when focused on one *(red #1 of 2 → red #2)*
- should jump to the first ring member when the focused session is not in the ring
- should never target a headless session
- should exclude headless from the ring entirely

Rows whose expectations change (C1–C5), named for the new behavior:

| Name | Sessions (focused in **bold**) | Want |
|---|---|---|
| should pop out to the idle tier when the focused red is the only red | **R**, O1, G1 | O1 |
| should pop out to the idle tier from the last red of several | R1, **R2**, O1 | O1 |
| should wrap from the last green back to the first red | R1, O1, **G1** | R1 |
| should wrap from the last idle to the first red when no green exists | R1, O1, **O2** | R1 |
| should cycle green when an unknown session is present | **G1**, G2, X | G2 |
| should treat a delegating session as a green ring member | **G1**, D1 | D1 |
| should pop out of the delegating tier into the wrap | G1, **D1**, R1 | R1 |
| should return nil when the only navigable session is focused | **R1** | nil |
| should return nil when the ring is empty | X, X | nil |
| should skip a sibling pane sharing the focused window | **G1**, **G2**, O1 | O1 |

New rows for the ring invariants:

| Name | Assertion |
|---|---|
| should visit every ring member exactly once per lap | drive N presses from a synthetic snapshot, flipping `Focused` to the returned target each time; assert the visited sequence is a permutation of the ring and returns to the start |
| should never return the focused session | property test over the table: `got == nil \|\| !got.Focused` |
| should order the ring permission then idle then green in snapshot order | assert `attentionRing` directly against a mixed snapshot |

`TestAggregateNavigationSkipsUnboundRemoteRows` (`:47`) calls
`pickAttentionExact([]state.Session{local, remote}, &local)` and asserts the
target is `local`. Under the ring, `local` is focused and would be skipped —
with the unbound `remote` excluded by `sessionNavigable`, the ring is `[local]`
and the answer becomes `nil`. Rewrite the assertion to that: *an unbound remote
row is never a target, and a lone focused local session yields no move.* Add a
sibling case with a second bound local so the aggregate path still proves it can
move.

---

## 4. Docs and help text

| File | Change |
|---|---|
| `cmd/switchboard-ctl/main.go:913` (usage) | rewrite the `attention` entry: *"advance one step around the attention ring — permission, then idle, then green, each in snapshot order. Always moves; wraps at the end. No-op only when there is nowhere else to go."* |
| `README.md:245` | replace *"first permission, else first idle, else cycle green if all green (repeat to cycle the tier)"* with the ring description |
| `README.md:377` | the "only root lines are navigation targets" note still holds; add that grey sessions are reachable via `cycle` but not `attention` |
| `docs/attention-latency-report.md` §3 R1 | supersede the "first tier with a non-focused member" sketch with this plan; keep R2 but demote it from **precondition** to **quality fix** — §2.3's skip loop makes the split-pane case merely a longer hop, not a missed RED |
| `docs/behavior-spec.md` | add the ring rule and the C1–C5 rows if the spec enumerates ctl commands |

---

## 5. Risks

| Risk | Severity | Mitigation |
|---|---|---|
| A red is now up to one lap away from a green session (C5) | medium | accepted trade, §1.2; the real remedy is R4/R5 so stale reds stop existing |
| Excluding grey from the ring loses the "half-discovered snapshot stays put" guard | low | the guard only ever protected the all-green fallback. A snapshot that is *entirely* grey still yields an empty ring and a no-op, which is the case the guard was written for |
| `delegating` joining the green tier makes a busy-by-proxy session a target | low | it already renders green and is a legitimate window to visit; C4 fixes a strictly worse behavior |
| Window-level `Focused` skips a whole split window | low | intended (§2.3); R2 narrows it to the pane |
| Federated/remote rows | none | `sessionNavigable` is unchanged and still gates on `Navigable` for rows carrying a `Hostname` |

---

## 6. Sequencing

One commit, no flag, no migration — the change is local to `switchboard-ctl` and
the binary is redeployed with `scripts/deploy`.

1. `attentionRing` + `nextAttentionTarget`, with the table test rewritten first
   (the C1–C5 rows fail against today's code, which is the point).
2. Delete `topAttentionTier`, `pickAttention`, `pickAttentionExact`,
   `sameExactSession`; simplify `cmdAttention`.
3. Usage text + `README.md` + the two doc updates in §4.
4. `go test ./...`, then `scripts/deploy` — **not** `go install`; a configured
   unit need not read `~/go/bin`.

**Definition of done.** On a live snapshot with two or more navigable sessions,
every `$mod+A` press changes the focused window. A singleton red level pops to
the next level in one press. N presses visit N distinct sessions and return to
the start. A grey Codex root never strands the key. The only no-op left is a
single navigable session, or none.
