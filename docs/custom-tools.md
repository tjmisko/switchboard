# Custom commands and tools in Claude Code

If you add your own slash commands, plugins or tools to Claude Code, the status
chip should still show the right color. This page explains how switchboard
reads a session, how a custom command or tool can fool it, and what to do about
it.

## The colors

| Situation | Color |
|---|---|
| The agent is working | green |
| The agent is done, or you ran a command and the agent is doing nothing | orange |
| The agent is stuck until you answer (a permission prompt, a question) | red |

The full model is in [status-color-state-model.md](status-color-state-model.md).

## Where the color comes from

Switchboard has two sources for a Claude session:

1. **Hooks.** Claude Code fires `UserPromptSubmit` when a turn starts, `Stop`
   when it ends, and `PermissionRequest` when it needs your approval. These set
   the color directly.
2. **The transcript.** Some changes fire no hook, such as an interrupted turn or
   a session resumed elsewhere. To catch those, switchboard also reads the end of
   the session's `.jsonl` transcript. If a new entry appears in an orange session,
   it reads that as "the agent started working again" and turns the chip green.

Custom commands and tools go wrong in the gap between these two sources.

## Case 1: a command that adds text but starts no turn → should be orange

**Example.** limpid's `/read` opens the last answer in a browser window so you
can read it and leave feedback. It returns a short note for the agent ("The user
opened limpid page …") and ends. No turn starts, and the agent does nothing until
you send feedback.

**What could go wrong.** A local command fires no hook. Its note still lands in
the transcript as a new entry. Switchboard would read that as the agent working
again and turn the chip green. With no `Stop` coming, it would then stay green
until your next turn.

**How it is handled.** Claude Code marks every entry it adds on its own with
`"isMeta": true`. Examples are a command's returned context, the reminder
`/rename` leaves, and a loaded skill. Switchboard never counts `isMeta` entries as
activity, so the chip stays orange.

**What you need to do.** Nothing, as long as your command hands its output back
to Claude Code. For a plugin command, that means returning it from the handler
(as limpid does with `return { context: [...] }`). Don't write to the transcript
file yourself; an entry without `isMeta` reads as the agent working.

If your command *does* start the agent, by submitting a prompt, the chip goes
green. That is correct: the agent is working. limpid's feedback works this way.
When you submit feedback, it arrives as a new prompt, `UserPromptSubmit` fires,
and the chip turns green.

## Case 2: a tool the agent calls that waits for you → should be red

**Example.** An MCP tool, or a script the agent runs, opens a window with a
question and doesn't return until you answer.

**What goes wrong.** The agent is in the middle of a turn, so the chip is green.
If the tool is pre-approved, Claude Code shows no permission prompt and fires no
`PermissionRequest`. Switchboard can't tell that the tool is waiting on you, so
the chip stays green while the session is blocked.

Switchboard gets this right for Claude Code's built-in `AskUserQuestion` (and
`ExitPlanMode`) because it knows those tool names. It does not yet know yours.

**What you can do today.** Pick one of these:

- **Ask through `AskUserQuestion`.** If the question fits a multiple-choice
  prompt, have the agent use the built-in tool. The chip turns red.
- **Return at once, answer later.** Design the tool so it opens its window and
  returns straight away, ending the agent's turn. Send the answer back as a new
  prompt when the user submits it. The chip is orange while you think, which is
  accurate: the agent is idle. limpid uses this design.

**Planned.** A config entry that names your tool as one that waits on the user,
so switchboard shows red for as long as the call is open:

```json
{ "tool_rules": [{ "provider": "claude", "tool": "mcp__review__ask", "while_running": "user_input" }] }
```

This is not built yet. Track it in
[#118](https://github.com/tjmisko/switchboard/issues/118).

## Quick check

Questions to ask about anything you add to Claude Code:

1. **Does it start an agent turn?** If yes, green is right.
2. **Does it add text to the conversation without starting a turn?** It must
   reach the transcript through Claude Code (as a command's output), so the entry
   is marked `isMeta`. The chip stays orange.
3. **Does the agent call it, and does it wait for you?** It needs to be visible as
   a question: use `AskUserQuestion`, return at once, or follow #118.
