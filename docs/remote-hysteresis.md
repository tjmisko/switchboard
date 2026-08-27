# Remote hysteresis — holding a host's last observation

Supersedes [`ssh-federation-plan.md` §4.3](ssh-federation-plan.md), which
removed a remote host's rows the moment its SSH stream died.

## The bug

An SSH stream's health and a remote session's existence are different facts.
The original design conflated them: any stream loss removed every row that
stream carried. On a link that drops a tick now and then, a two-second
reconnect blanked a whole host's chips off the waybar and put them back —
repeatedly, and most visibly on exactly the busy hosts the bar exists to watch.

Worse, the conflation was not even conservative in the other direction. SSH
declared a dead link only after `ServerAliveInterval × ServerAliveCountMax`
(30s as configured), and the daemon publishes only on a real change — so an
idle remote could be unreachable for half a minute while every row claimed, with
no qualification whatsoever, to be current.

## The rule

Answer two questions separately.

**Did the peer say it was going away?** Then believe it. `remote-stream` writes
a final closeout frame and the client drops the host's rows at once.

**Or did we merely stop being able to hear it?** Then hold the last observation,
and say so once holding it stops being free.

## Closeout

```json
{"host":"buildbox","closeout":{"reason":"signal"}}
```

Emitted by `switchboard-ctl remote-stream` on SIGTERM/SIGINT/SIGHUP — a
deliberate teardown, which is what a host shutdown and a manual stop both look
like. It is best effort by construction: on the commonest teardown path (the SSH
client hanging up) stdout is already broken and the write fails. It is worth
attempting anyway, because the case it *does* reach — a machine being shut down
while the link is still up — is precisely the one where waiting out a hold
window would feel broken.

It is deliberately **not** emitted when the remote's own daemon socket drops. A
`systemctl restart switchboard` there leaves every agent session on that machine
running; closing out would blank the client's chips for exactly the restart the
hold exists to cover. (`started_at` survives that restart — the daemon rehydrates
it from its own `state.json` — so the rows that come back are the same rows,
with the same routes.)

The design invariant: **a closeout can only ever restore the pre-hold behavior**
(drop now, reconnect if you can). It can never remove rows that would otherwise
have survived, so a peer that emits one too eagerly is no worse than a peer that
cannot emit one at all.

## The hold

| Phase | Window | Published state |
|---|---|---|
| quiet | 0 – `-remote-quiet` (6s) | nothing at all — not even a "stale" edge |
| stale | up to `-remote-hold` (45s) | rows held, `stale: true`, `last_contact` stamped |
| dropped | at `-remote-hold` | host removed, routes invalidated |

The quiet window is sized against the reconnect path it covers: a worker waits
`DefaultRetryDelay` (2s) then pays one SSH connect. Publishing *anything* during
it — including a stale marker — would trade a disappearing chip for a blinking
one, so it publishes nothing.

`-remote-hold 0` restores immediate removal.

### A held row stays navigable

This is the point, not an oversight. What a remote chip focuses is the **local**
terminal pane displaying that SSH session. Losing the state stream says nothing
about where that window is, and losing contact is exactly when a user is most
likely to want to go and look. So `OnHostRemoved` — the route/focus invalidation
edge — fires at the *end* of the hold, not at the disconnect.

### Staleness is stamped on the outgoing copy

`Manager.Snapshot` returns rows already carrying their host's verdict, rather
than exposing a second host-liveness map. A consumer that had to read rows and
liveness in two calls could tear between them and render a host as both live and
gone. One read, one answer.

`last_contact` is the **client's** clock. A remote `updated_at` is a different
machine's wall clock — not a causal revision, not differenceable against a local
instant — so the only honest statement available is how long *this* machine has
been out of touch.

## Which failures hold, and which do not

| Read outcome | Class | Result |
|---|---|---|
| EOF, read error, non-zero `ssh` exit | transport | hold |
| final frame cut mid-line (`ErrTruncatedFrame`) | transport | hold |
| unclassified error | transport | hold |
| schema mismatch, invalid/oversized frame | protocol | drop now |
| duplicate hostname, hostname flip, local-hostname claim | protocol | drop now |
| `closeout` frame | closeout | drop now |

A peer we cannot parse will still be unparseable after the reconnect, so holding
rows we can never refresh would be a lie with only a timer to end it.

The truncated-frame case is why `ReadFrames` decodes complete lines only.
`EncodeFrame` marshals whole and always terminates, so an unterminated tail is
the transport's doing and never the encoder's — reporting it as a malformed
frame would libel the peer *and* drop rows that should have been held.

## Keepalive

The local daemon publishes only on a real change, so an idle host emits nothing
for minutes and a reader cannot tell a healthy quiet stream from a black hole.
`remote-stream` therefore re-sends its current snapshot every 10s and advertises
that period:

```json
{"host":"buildbox","snapshot":{…},"keepalive_seconds":10}
```

A client that has heard that advertisement marks rows stale after
`3 × keepalive` of silence — it never *drops* on silence alone, because SSH is
the authority on whether the link is dead and a mis-parsed advertisement must
not be able to delete a healthy host. A peer that advertises nothing (an older
remote) gets transport-level detection only, exactly as before.

The keepalive is a verbatim **re-send**, not a new message type, and that is
what makes it invisible to an older client: it sees an ordinary frame carrying
state it already has. There is no protocol version to negotiate — which matters,
because the transport is one-way (`ssh -n`) and there is nowhere to negotiate
one. On the client side `state.ObservablyEqual` — the same comparison the local
publish gate uses — suppresses the redundant broadcast, so a quiet host costs
one small frame per period and no bar churn at all.

Because holding absorbs a disconnect, the client can afford to *detect* one
faster: `ServerAliveInterval` is 5 with `ServerAliveCountMax` 3, declaring a dead
link in ~15s instead of ~30s. Fast detection and hysteresis are complements, not
alternatives.

## Cross-version behavior

The transport is one-way, so every compatibility property has to be structural.

| Client | Remote | Result |
|---|---|---|
| new | new | everything above |
| new | old | no keepalive advertised → no silence detection; hold still works, driven by SSH |
| old | new | keepalive frames decode as ordinary snapshots it already has; unknown fields ignored |
| old | new, closing out | closeout has no `snapshot`, so the old client rejects the frame and tears the stream down — which is what the closeout was asking for, one log category off (`invalid_frame` instead of `closeout`) |

## Races the epoch fence closes

Removal used to be performed by the owning worker itself, immediately after its
read loop ended, so nothing could publish for that host concurrently. Moving
removal onto a timer breaks that: a reconnect can now race an expiry that is
already inside its `OnHostRemoved` callback.

Every event that changes what a host's deadlines mean bumps that host's epoch,
and a removal is fenced by the epoch it was decided under. A removal that finds
the epoch moved abandons itself — and **republishes**, because the reconnect's
own publish may have raced ahead of the route invalidation, leaving the live
route projection holding a map it has already acted on. The republish makes it
re-read the current one and restore what the callback tore down.

## Configuration

| Flag | Default | Meaning |
|---|---|---|
| `-remote-hold` | `45s` | how long a host's last observation stands with no contact; `0` removes rows at the disconnect |
| `-remote-quiet` | `6s` | how much of the hold passes with no observable change before rows are marked stale |

Bars read `stale` (boolean, omitted when false) and `last_contact` (RFC 3339,
present only on stale rows) on aggregate rows. Like `hostname`, `remote`, and
`navigable`, they appear only on a federated client's detached aggregate copies
and never in host-local `state.json`, so the frozen schema is unchanged.
