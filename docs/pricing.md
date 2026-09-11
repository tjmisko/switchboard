# Usage accounting and cost estimates

Switchboard records canonical Claude and Codex usage, then prices it in
`switchboard-ctl timeline --json`. The dashboard consumes the structured `cost`
objects on lanes, totals, and the rolling plan window. `cost_usd` remains a
deprecated nullable alias; consumers must not treat it alone as a supported
estimate.

Claude accounting deduplicates streamed assistant fragments by message identity,
tracks exact child transcripts, and keeps cache-write TTL buckets and server-tool
units separate. Codex accounting uses exact hook-bound rollout paths and durable
usage cursors. App-server usage enriches the graph; it is not counted a second
time in history. Collectors acknowledge durable history before advancing cursors.
History recording continues even when an unchanged state suppresses publication.

`usage_sample` events with `usage_event_id` are full replacement snapshots:
the greatest `usage_revision` wins. Legacy samples without an identity remain
additive. `usage_cutover` marks incomplete collection. Cumulative vendor credits
and USD estimates use separate `vendor_usage_snapshot` events and are never
added to token-based API equivalents.

## Price freshness

The daemon refreshes public Anthropic and OpenAI catalogs at startup and every
six hours. Timeline reads use one immutable cached catalog set for the whole
response and make no network calls.

```sh
switchboard-ctl pricing status --json
switchboard-ctl pricing refresh --json
```

The cache lives under `$XDG_CACHE_HOME/switchboard/pricing`, defaulting to
`~/.cache/switchboard/pricing`. Diagnostics identify the source, retrieval time,
content hash, model count, freshness, and fallback use. Failed refreshes retain
the last valid cache. Catalogs become stale after 24 hours and unusable after
seven days; bundled catalogs are always marked stale and obey the same cutoff.

Missing prices, unsupported models, ambiguous billing routes, and incomplete
tool coverage remain explicit. Unknown dollar amounts are null. API equivalents
compare observed usage with public rates; they do not establish actual billed
spend or subscription inclusion. Codex token-only estimates remain partial.

## Deployment compatibility

Deploy the daemon and CLI from the same reconciled revision. The dashboard's
provider registry overrides its `--ctl` flag, so its Switchboard entry must
resolve through the same `~/.local/share/switchboard/current` release as the
daemon. A private copy under `~/.config/switchboard/bin` will drift after a normal
Switchboard deployment.

Before activation, run the dashboard's cross-repository check against the exact
CLI binary being shipped:

```sh
SWITCHBOARD_TEST_CTL=/absolute/path/to/switchboard-ctl \
  go -C ../switchboard-dashboard test -run TestPricingProviderContract .
```

It exercises actual subprocess output through the single-provider HTTP path and
the multi-provider merge, including revised usage and unknown models. Full live
acceptance also requires current catalogs and verification of the running service
binaries. Memory sampling remains retired; the reconciliation does not restore
its sampler, payload fields, or CLI command.
