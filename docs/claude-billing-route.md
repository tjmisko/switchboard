# Claude Code billing route: the problem and a proposed fix

Status: problem statement and proposal, nothing implemented. 2026-10-12.

## The problem

Switchboard cannot tell whether a Claude Code session was paid for by a
subscription plan, an Anthropic API key, a gateway such as OpenRouter, or a
cloud provider. Every Claude usage event is recorded with model, service tier,
speed and inference geography from the transcript, and nothing about the
route (`cmd/switchboard/fanout.go:159-162` says so explicitly). The pricing
layer therefore marks every Claude estimate `partial` with "execution provider
inferred from exact model; billing route is unconfirmed"
(`internal/pricing/calculate.go:187-192`).

The consequence is that the dashboard cannot answer the questions that
matter:

- How much of today was plan usage (no incremental dollars) versus metered
  usage that is actually billed?
- Is a session I believe is on the plan quietly billing an API key?
- What did the OpenRouter sessions cost, at OpenRouter's prices rather than
  Anthropic list prices?

For an operator who only ever uses the plan, every Claude dollar figure is
an API-equivalent value, not a charge. Today the UI cannot say that with
confidence, and it cannot warn when it stops being true.

## Why the obvious signals are wrong

Claude Code picks one credential by a documented precedence
([authentication precedence](https://code.claude.com/docs/en/authentication#authentication-precedence)):
cloud-provider flags, then `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`,
`apiKeyHelper`, `CLAUDE_CODE_OAUTH_TOKEN`, Anthropic profiles, and finally the
`/login` subscription. Each tempting shortcut misclassifies some session:

| Shortcut | Why it fails |
| --- | --- |
| `ANTHROPIC_BASE_URL` points at `openrouter.ai`, so OpenRouter | A base URL alone does not replace the subscription. A saved claude.ai login stays active and its billing applies ([llm-gateway](https://code.claude.com/docs/en/llm-gateway#subscriptions-and-gateways)). Verified below. |
| `ANTHROPIC_API_KEY` is set, so API | In interactive mode the key needs one-time approval. A declined key is ignored and the session stays on the plan ([llm-gateway-connect](https://code.claude.com/docs/en/llm-gateway-connect#troubleshoot-gateway-errors)). Where that decision is stored is undocumented. |
| Read the process's `/proc/<pid>/environ` | Values from the `env` block in `settings.json` are applied inside Claude Code and do not appear in the initial environment. |
| `~/.claude/.credentials.json` exists, so plan | A login can exist while a higher-precedence credential is in use. The file format is undocumented and holds secrets. |
| Nothing is set, so plan | True for this machine today, but it is an assumption about configuration, not evidence about the session. |

## What is observable

`claude auth status --json` (Claude Code 2.1.296) reports the credential it
would use under the environment it is run in. It takes about 80 ms, works
offline, and prints no secrets. Probed on this machine with dummy credentials
(nothing was sent anywhere):

| Environment | `authMethod` | `apiProvider` | `subscriptionType` |
| --- | --- | --- | --- |
| (none) | `claude.ai` | `firstParty` | `max` |
| `ANTHROPIC_BASE_URL=https://openrouter.ai/api` only | `claude.ai` | `firstParty` | `max` |
| base URL + `ANTHROPIC_AUTH_TOKEN` | `oauth_token` | `firstParty` | null |
| base URL + `ANTHROPIC_API_KEY` | `api_key` | `firstParty` | null |
| `ANTHROPIC_API_KEY` only | `api_key` | `firstParty` | null |
| `CLAUDE_CODE_OAUTH_TOKEN` only | `oauth_token` | `firstParty` | null |
| `CLAUDE_CODE_USE_BEDROCK=1` | `third_party` | `bedrock` | null |

Two limits matter:

1. **`oauth_token` is ambiguous.** A plan's long-lived token
   (`CLAUDE_CODE_OAUTH_TOKEN`, from `claude setup-token`) and a gateway bearer
   (`ANTHROPIC_AUTH_TOKEN`, how OpenRouter is usually configured) report the
   same value, and `apiProvider` stays `firstParty` behind a gateway. The
   base-URL host and which variable is set must break the tie.
2. **The API-key approval is not visible to `auth status`.** It reports
   `api_key` whenever the variable is set, even though an interactive session
   whose user declined the key runs on the plan. Configuration cannot settle
   this; evidence from the session's own responses can (next section).

### Evidence from the request path: statusline `rate_limits`

The statusline command receives a JSON object per session, including
`session_id`. Its `rate_limits` field is filled from the responses to the
session's own requests, and appears only for claude.ai Pro/Max subscribers or
behind a gateway with spend limits, after the first API response
([statusline](https://code.claude.com/docs/en/statusline#available-data)).
Unlike everything above, this is observed from requests that were actually
billed, not inferred from configuration:

- `rate_limits` present, no gateway host: the session is on the subscription,
  whatever `auth status` said. This settles the declined-key case.
- a response seen, `rate_limits` absent, no gateway host: not the
  subscription; with `api_key` or `api_key_helper`, the key was used.
- no response yet: undecided. The route stays at its SessionStart value
  marked `pending` until the first statusline update after a response.

The existing statusline script (`~/.claude/claude-status`) already reads
`rate_limits`. It forwards one boolean per session to `switchboard-ctl`,
once, not on every refresh.

Hook subprocesses inherit Claude Code's environment, including `settings.json`
`env` values ([hooks](https://code.claude.com/docs/en/hooks): "A hook process
inherits the parent environment"; the `env` block applies to "every session
and its subprocesses"). The SessionStart hook, which already runs
`switchboard-ctl`, is therefore the one place that sees the session's real
credential environment. This should be confirmed with a test hook before
anything depends on it.

## Proposal

### 1. Detect at SessionStart, in the hook process

On SessionStart (fired on startup, resume, `/clear` and compaction, so a
mid-session `/login` is picked up at the next one), `switchboard-ctl`:

1. Runs `claude auth status --json` with its own inherited environment.
2. Reads, by name only, which route variables are present, plus the *host* of
   `ANTHROPIC_BASE_URL`. No value other than that host is read or sent.
3. Classifies:

   | Evidence | Route | Execution provider |
   | --- | --- | --- |
   | `claude.ai` | `subscription` (+ `subscriptionType`) | `anthropic` |
   | `oauth_token`, `CLAUDE_CODE_OAUTH_TOKEN` set, no gateway host | `subscription` | `anthropic` |
   | `oauth_token` or `api_key`, gateway host (not `api.anthropic.com`) | `gateway` | the host, e.g. `openrouter` |
   | `api_key` / `api_key_helper`, no gateway host | `api`, confirmed or overturned by statusline evidence | `anthropic` |
   | `third_party` | `cloud` | `apiProvider` (`bedrock`, `vertex`, `foundry`) |
   | anything else, or the probe failed | `unknown` | unset |

4. Sends the classification and its evidence (method, provider, subscription
   type, variable names, gateway host) to the daemon. The daemon never receives
   environment values or credentials.

The hook must not slow Claude Code's startup. The probe runs with a short
timeout and, if it times out, the hook reports `unknown` instead of waiting.

### 2. Preserve the whole route, not one provider

A request can pass through several parties, and each one matters:

```
client  ->  gateway     ->  upstream provider  ->  model
claude      openrouter      anthropic              claude-opus-5-5
pi          openrouter      deepinfra              deepseek/deepseek-v4.1-flash
codex       (none)          openai                 gpt-5.6-sol
```

`execution_provider` alone flattens this: "openrouter" hides who served the
request, and "anthropic" hides that OpenRouter billed it. The identity gains a
`gateway` field beside `execution_provider`, where `execution_provider` is
always the party that ran the model, and `billing_route` says who charged
(`subscription`, `api`, `gateway`, `cloud`). The model name's vendor prefix
(`deepseek/`, `anthropic/`) is the model's author, not the provider, and must
not fill `execution_provider`.

What each client exposes today:

- **Pi** records, per assistant message, `provider: "openrouter"`, the
  prefixed model, its own cost computed from Pi's price table, and
  `responseId` (the OpenRouter generation id, `gen-...`). It drops the
  upstream provider that OpenRouter's response names. Two ways to recover it:
  ask Pi to keep the response's `provider` field, or look up the generation
  id, which returns the upstream provider and the amount OpenRouter actually
  charged. The lookup needs the operator's OpenRouter key, so it must be opt-in
  and run where the key already lives, never copied into Switchboard.
- **Claude Code** through OpenRouter has not been observed here. Whether its
  transcript keeps the generation id (as the message id) must be checked with
  one real session before designing on it.
- **Codex** reports `model_provider`, the configured provider entry; an
  OpenRouter entry needs the same gateway/upstream split.

When the upstream is unknown, `gateway` is still recorded and
`execution_provider` stays empty. It is never guessed from the model name.

### 3. Record it once per session, not per event

A new `session_identity` history event carries the session id, agent kind,
birth token, route, execution provider and evidence. Usage events stay as
they are. The timeline joins usage to the session's identity when pricing.
This keeps the usage log small and lets a later correction (a re-detection on
resume) apply to the whole session.

### 4. Let the operator declare the expected route, and check it

Detection answers what happened; a declaration says what should happen. In
`~/.config/switchboard/billing.json`:

```json
{ "claude": { "expected_route": "subscription" } }
```

| Detected | Declared | Result |
| --- | --- | --- |
| matches | any | `included` (subscription) or `estimated` (api, gateway, cloud), provenance `detected` |
| differs | set | priced by the detected route, plus a visible mismatch warning: "expected subscription, session used api" |
| `unknown` | set | priced as declared, provenance `declared` and shown as such, never as detected |
| `unknown` | unset | `partial`, as today |

A mismatch is shown in the dashboard only, never in the status bar.

The mismatch row is the point of the declaration. For a plan-only operator it
turns "probably included" into "included, and you will be told otherwise".
It also gives sessions recorded before detection existed an honest label:
`declared`, not `detected`.

### 5. Price each route by what it actually charges

| Route | `api_equivalent_usd` | `estimated_billed_usd` | Status |
| --- | --- | --- | --- |
| subscription | Anthropic list price, shown as value | null | `included` |
| api | Anthropic list price | same | `estimated` |
| gateway | upstream list price when the upstream is known, labelled | the gateway's charged amount from the generation lookup; otherwise null | `partial` until the charged amount is known |
| cloud | first-party list price, labelled | null until a cloud catalog exists | `partial` |

OpenRouter publishes per-model prices at a public endpoint. That catalog is a
separate source, in the same shape as the Anthropic and OpenAI sources, and
outside this proposal.

## Other clients

- **Codex** already classifies ChatGPT login versus API key versus Bedrock
  from app-server telemetry (`internal/provider/codex/telemetry.go`). A
  non-OpenAI `model_provider` should map to the `gateway` route the same way.
- **Pi** reports its provider (for example `openrouter`) and its own cost, but
  never sets `billing_route`. Its provider string should map to a route through
  the same table.

## Decisions

- A route mismatch appears in the dashboard only, never in the status bar.
- An approved versus declined API key is decided by statusline evidence, not
  assumed.
- The route is preserved end to end: gateway and upstream provider are
  separate fields, and an unknown upstream stays unknown.

## Open questions

1. Generation lookup for gateway sessions: acceptable as an opt-in that runs
   with the operator's OpenRouter key, or should Pi be changed to record the
   upstream provider instead?

## Tests the implementation needs

- A classification table test covering every row above, including base URL
  alone (subscription) and both meanings of `oauth_token`.
- The probe's timeout path reports `unknown` without delaying the hook.
- Statusline evidence: `rate_limits` present overturns an `api_key`
  classification to subscription; absent after a response confirms it; no
  response leaves it `pending`; repeated statusline refreshes send one event.
- Route preservation: a Pi OpenRouter message yields gateway `openrouter`
  and an empty `execution_provider`, never `deepseek` from the model prefix.
- No environment value other than the base-URL host reaches the daemon or
  history (assert on the serialized RPC request and event).
- Declared/detected/mismatch resolution, including sessions with no
  `session_identity` event.
- A manual check that a `settings.json` `env` value reaches the SessionStart
  hook before relying on it.
