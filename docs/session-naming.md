# Session naming & project prefixes

switchboard gives both Claude and Codex roots a stable task name, prefixes it
with a short **project abbreviation** derived from the session's directory —
`arachne-…`, `sspi-…`, `sb-…` — and de-duplicates so you never get
`arachne-arachne-…`.

Codex terminal titles are never used as Codex chip labels. An unnamed Codex
thread can put its UUID in the title, and its configurable activity item paints
animated spinner frames there. Feeding that title into the bar made both details
visible and made the chip's identity move. Switchboard instead uses stable
app-server thread metadata.

## The two-layer model

A Claude session has two independent "names":

1. **Claude's own name** — what shows in Claude's input-box border and the
   `/resume` picker. It is set by `/name <text>` (an alias for `/rename`) or by
   `claude -n <text>` at launch, and stored as `custom-title` / `name` records
   under `~/.claude/`. **Only Claude can change the live value** — external file
   writes do not move it (verified empirically; see below).

2. **switchboard's label** — the bottom-bar chip text and hover. switchboard
   computes this at **display time** from the session's cwd, so it is always
   project-prefixed regardless of what you `/name` the session.

Because layer 2 re-derives the prefix every render and de-duplicates, **`/name`
never fights the scheme**:

| you type          | chip shows           |
| ----------------- | -------------------- |
| `/name foo`       | `arachne-foo`        |
| `/name arachne-foo` | `arachne-foo` (no double) |
| `/name arachne`   | `arachne`            |

The launcher wrapper (layer 1) is a bonus: it prefixes `claude -n` at startup so
Claude's *own* border is also project-scoped from the first frame. If you
`/name` afterward, Claude's border follows your text; switchboard's chip stays
prefixed either way.

### Why not auto-prefix Claude's live name?

A throwaway-session experiment (v2.1.190) showed the live displayed name comes
solely from Claude's in-memory state. Appending `custom-title` to the transcript
**and** editing `~/.claude/sessions/<pid>.json` `name`, then driving a turn, left
the live border unchanged (the on-disk values persisted but were not read back
into the running UI). So there is no supported or unsupported way to
auto-prefix a *running* session's own name — hence the display-time approach.

## Codex names

Ordinary `codex` sessions receive a Switchboard-owned display label after
their first usable completed turn. `UserPromptSubmit` retains a bounded prompt
candidate for the exact process lifetime and conversation; the matching later
`Stop` supplies the bounded final assistant message. Empty or interrupted
completions are discarded, and the next completed turn gets another chance.

The naming turn receives only the cwd basename, prompt, and final response. Both
content fields are limited to 1,000 Unicode characters and remain ephemeral.
The selected model must return a lowercase 2–5-word kebab-case title no longer
than 40 characters. Switchboard retries once, then chooses a deterministic
fallback. The model defaults to `gpt-5.6-luna` and is configurable with
`-codex-autoname-model`.

Only this record is persisted:

```json
{
  "value": "context-aware-session-names",
  "origin": "generated",
  "conversation_id": "<codex-thread-id>",
  "native_baseline": "session-naming"
}
```

No prompt, assistant response, pending attempt, or turn ID enters
`state.json`, history, diagnostics, or logs. The record is valid only for its
bound conversation. A daemon restart preserves a valid record for the same
live conversation but never reconstructs discarded naming context.

Codex label precedence is:

1. a valid Switchboard `display_name`;
2. the current native app-server root name;
3. the first two characters of the exact thread ID;
4. the cwd basename;
5. `pid N`.

Whichever value wins is normalized at the display boundary to a lowercase,
at-most-three-word kebab slug. The persisted display record and Codex's native
name remain untouched. Project prefixing happens after normalization. For
example, a native `Plan faster abstract extraction` in Lysilogy renders as
`lysilogy-plan-faster-abstract`.

Switchboard never mutates Codex's native name. When the read-only observer sees
an authoritative native name, that value becomes the display record's baseline.
A later authoritative value that differs clears the generated record, so
`/rename` takes precedence. Partial hook graphs and unavailable observations
cannot manufacture a rename. With the observer disabled, generation still
works; rename precedence is simply delayed until authoritative metadata becomes
available.

Codex terminal titles are not naming inputs. Their spinner, model, branch, and
full UUID therefore cannot leak into a chip label.

## Components

- **`internal/projectname`** — the pure resolver: prefix + dedup (longest-alias,
  hyphen-boundary), git-root detection, and a writable user config layered over
  built-in defaults.
- **`switchboard-ctl name`** — `resolve --cwd --name`,
  `abbrev --cwd` (current abbreviation), `set <dir> <abbrev>` (persist).
- **`internal/label`** — sources Claude names from
  `~/.claude/sessions/<pid>.json` (with its existing terminal-title fallback)
  and Codex names from a conversation-bound `display_name` or the app-server
  root node, then applies the compact-ID fallback and project prefix. Shared by
  the Waybar chips and `switchboard-ctl pick`/`list`.
- **`internal/provider/codex`** — generates validated display labels in an
  isolated ephemeral turn and observes native names through a read-only graph.
- **Claude startup wrapper** — `~/.config/scripts/claude-name-wrapper.sh`.
- **hover rename** — middle-click a chip → `~/.config/scripts/claude-abbrev-edit`.

## Abbreviations & new projects

Abbreviations live in `~/.config/switchboard/projects.json`, keyed by absolute
git-root path, layered ahead of the built-in defaults (matched by basename):

```json
{ "projects": { "/home/you/Projects/Arachne": { "canonical": "ar", "aliases": ["ar"] } } }
```

Built-in defaults: `arachne`; `sspi-data-webapp → sspi` (aliases
`sspi`/`sspi-data`/`sspi-data-webapp`); `switchboard → sb` (aliases
`switchboard`/`switch`/`sb`).

**A project you have never named just works**: its abbreviation falls back to the
sanitized git-root basename (`~/Resume → resume`), shown immediately with no
prompt. Rename it whenever you like:

- middle-click its chip and type a new abbreviation (rofi), or
- `switchboard-ctl name set <dir> <abbrev>`.

The bar re-renders on the next daemon snapshot (~1s).

## Activation

```sh
go install ./...                              # rebuild switchboard-ctl + switchboard-waybar
echo 'source ~/.config/scripts/claude-name-wrapper.sh' >> ~/.bashrc   # optional layer-1 wrapper
# restart the bottom bar so waybar picks up the new claude.jsonc bindings
switchboard-ctl bottombar stop && switchboard-ctl bottombar reconcile
```
