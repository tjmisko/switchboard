# Codex session naming findings

## Decision

Switchboard owns a display-only label for ordinary `codex` sessions. Naming
starts after the first usable completed turn, uses bounded hook context, and
persists only the validated label and its provenance. Codex's native thread name
is never mutated.

This keeps the supported path identical to the user's normal workflow:

```text
codex
  -> trusted UserPromptSubmit / Stop hooks
  -> Switchboard completed-turn reducer
  -> isolated ephemeral naming turn
  -> Session.display_name
```

## Verified inputs and boundaries

The trusted hook payload supplies the exact conversation ID. For naming, only
these two event fields cross the hook boundary:

| Event | Ephemeral field | Bound |
|-------|-----------------|-------|
| `UserPromptSubmit` | `prompt` | 1,000 Unicode characters |
| `Stop` | `last_assistant_message` | 1,000 Unicode characters |

The hook client forwards neither field for other events or providers.
Malformed payloads still produce a harmless content-free hook request. Raw
prompt, assistant, tool-input, and question content never enters persisted
state, history, diagnostics, or logs.

The naming request is structured as:

- cwd basename;
- bounded user prompt;
- bounded final assistant response.

The default model remains `gpt-5.6-luna`. Output must normalize to lowercase
2–5-word kebab-case and fit within 40 Unicode characters. There are two model
attempts; a deterministic content-local fallback is used afterward.

## Completed-turn reducer

Candidates are keyed by the process lifetime `(pid, started_at)`, exact Codex
conversation ID, and optional turn ID.

1. `UserPromptSubmit` retains a bounded candidate.
2. A newer prompt replaces an incomplete candidate.
3. `Stop` must be chronologically later and have a nonempty final assistant
   message.
4. When both hooks carry a turn ID, they must match. If either is absent, the
   first later `Stop` for the conversation is accepted.
5. Empty or interrupted completion discards the candidate.
6. Duplicate, stale, mismatched, or out-of-order hooks cannot start another
   attempt.
7. A newer completed attempt cancels older naming work.

A result commits only if the same process lifetime and conversation are still
live and no valid record already exists. Rotation after `/clear`, process
death, PID reuse, daemon shutdown, or a newer completed attempt cancels pending
work. Late results are discarded.

## Persisted state

Schema v3 adds one optional session field:

```json
{
  "display_name": {
    "value": "context-aware-session-names",
    "origin": "generated",
    "conversation_id": "<codex-thread-id>",
    "native_baseline": "session-naming"
  }
}
```

`origin` is `generated` or `fallback`. `native_baseline` is omitted until
an authoritative native observation is available; an authoritative empty name
is represented as an explicit empty string.

Schema-v2 mirrors are ignored. Discovery and hooks rebuild live sessions. After
restart, a valid display record survives only for the same live conversation.
If no record exists, Switchboard waits for the next completed turn instead of
reconstructing discarded context.

## Rendering and native rename precedence

Codex session labels resolve in this order:

1. valid, conversation-matching Switchboard display name;
2. authoritative native graph nickname;
3. two-character conversation ID;
4. cwd basename;
5. PID.

Project prefixing is applied afterward.

A read-only app-server observer may bind only from an exact hook-supplied thread
ID. It can read the root and descendants and consume native-name notifications;
it has no visible-thread mutation path.

The native value visible when generation commits becomes
`native_baseline`. If the current graph is a hook overlay, a carried nonempty
nickname remains trustworthy because hook payloads cannot supply nicknames; an
authoritative empty value waits for the next complete observation. A later
complete native observation differing from the baseline clears the display
record, allowing `/rename` to take precedence. Partial or unavailable
observations cannot clear it.

Display generation is independent of the observer. With observation off or
temporarily unavailable, hooks still generate the label. Native-rename override
is delayed until authoritative name metadata becomes available.

## Diagnostics and history

Naming diagnostics use finite content-free categories:

- `generated`;
- `fallback`;
- `canceled`;
- `stale-result`;
- `native-override`.

The existing label observer records one `session_label` history event for each
visible label change. It sees native → generated → later native transitions
without receiving the source prompt or assistant response.

## Acceptance evidence

Coverage pins:

- Unicode truncation and malformed hook payloads;
- matching, missing, mismatched, duplicate, and out-of-order turn IDs;
- empty completion, candidate replacement, retry, timeout, fallback, and
  exactly-once generation;
- `/clear`, process death/PID reuse, shutdown, supersession, and stale-result
  fencing;
- schema-v3 round trips, schema-v2 rejection, conversation binding, and absence
  of transient context;
- display/native/short-ID rendering and project prefixing;
- baseline preservation, partial-overlay immunity, and authoritative native
  override;
- exact read-only app-server binding with no mutation request;
- one history label event per visible change.

The complete verification gate is `go test ./...`, `go build ./...`, the
golden schema tests, and repository searches confirming that the retired Codex
identity/control surfaces are absent.
