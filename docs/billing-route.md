# Billing route detection

`switchboard-ctl hook SessionStart` runs `claude auth status --json` with a
one-second limit and classifies the auth method and `ANTHROPIC_BASE_URL` host.
The hook sends only gateway, upstream, route, and evidence labels to the daemon.
It does not send an auth token, email, full URL, or raw auth response. The daemon
records those labels as `session_identity`; usage joins to that event when read.

Claude's auth method alone cannot prove that a key was used for a response.
The statusline supplies a second signal after the first response. Add this
line immediately after `input=$(cat)` in the configured statusline script,
using the installed release path for `switchboard-ctl`:

```bash
printf '%s' "$input" | "$HOME/.local/share/switchboard/current/switchboard-ctl" billing-statusline >/dev/null 2>&1 &
```

The helper sends only `rate_limits_present` and a session identifier. It keeps
a marker under `$XDG_RUNTIME_DIR/switchboard/billing-evidence` to send once per
session. Direct Anthropic traffic with rate limits is classified as plan usage;
without rate limits after the response, it is classified as API usage. Gateway
traffic retains its gateway route. An unconfirmed API-key classification stays
unknown until the first response signal arrives.

Pi's Switchboard extension opts OpenRouter requests into response routing
metadata and forwards only the selected provider label when the response
includes one. If the metadata is absent, the upstream stays unknown. Pi's
client, gateway, upstream, and route are distinct fields in history. Codex
records the same fields from its existing billing identity observations.

The optional `~/.config/switchboard/billing.json` expectation is read by the
dashboard only. For example:

```json
{ "claude": { "expected_route": "subscription" } }
```

An expectation produces a warning when it differs from the measured route.
It never changes the route or price attached to usage.
