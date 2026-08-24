# Retrospective: standard Codex interview attribution

## User requirement

Switchboard must work when the user starts an ordinary `codex` TUI. Trusted
hooks are the exact identity and immediate lifecycle boundary. Optional
app-server observation may enrich that same root but cannot change how the user
starts Codex.

## What failed

An earlier design made full observation depend on a separate process topology
and registration/control protocol. Ordinary sessions therefore kept only the
partial hook graph. The implementation could demonstrate rich status in its
special topology while leaving the actual supported workflow unresolved.

That was a scope failure, not merely a missing setup note. A prerequisite that
changes the command the user runs changes the feature being delivered.

The design also conflated three concerns:

- OS discovery and navigation of a visible process;
- exact attribution of a Codex conversation;
- optional structured observation of that conversation.

When attribution was unavailable to the observer, the design tried to solve it
by changing process topology instead of preserving hooks as the authority.

## Corrected architecture

The standard path now has one identity flow:

```text
ordinary codex process
  -> discovery establishes (pid, started_at)
  -> trusted hook supplies exact conversation ID
  -> hook reducer publishes immediate partial status
  -> generic observer may read only that exact ID
```

The observer performs no independent loaded-thread discovery and cannot mutate
a visible thread. If it is off or unavailable, discovery, navigation, hook
status, and display-name generation continue to work. Structured child graphs
and native-rename override resume when authoritative observation returns.

`/clear` is a conversation rotation within the same process lifetime. The
new trusted hook identity retires the old one, clears conversation-bound data,
and fences late events. PID reuse is separately fenced by `started_at`.

## Interview wait lesson

Plan-mode `request_user_input` cannot be reduced from a generic active/idle
snapshot alone. The trusted hooks therefore own a correlator latch:

1. matching `PreToolUse` opens a user-input wait;
2. matching `PostToolUse`, the turn's `Stop`, or conversation rotation closes
   it;
3. unrelated tool activity and generic snapshots cannot clear it.

Only lifecycle IDs and tool identity cross the boundary. Question and answer
content are excluded.

## Session naming lesson

First-prompt naming was also premature: it described the request before the
turn's outcome was known and encouraged native-state mutation. The corrected
flow waits for a usable completed turn, combines bounded prompt and final
assistant response, and persists only a Switchboard display record. A later
authoritative native rename clears that record.

## Process corrections

Future provider work must start with a command-level acceptance statement:

> A fresh ordinary `codex` process, with documented hooks configured, reaches
> the promised behavior without any alternate invocation or manual registration.

Every richer observation tier must be tested as an optional enhancement over
that baseline. Documentation may explain degradation, but cannot redefine the
baseline around the mechanism that was easiest to instrument.

The regression suite now includes:

- exact hook binding for an ordinary process;
- no cwd/recency/loaded-thread attribution;
- hook-only interview waits;
- observer-disabled display generation;
- conversation rotation and retired-event fencing;
- full fake app-server enrichment for the exact hook ID;
- no visible-thread mutation request;
- later native rename precedence.
