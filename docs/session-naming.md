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

The locally verified Codex 0.149 app-server `Thread` object carries two naming
inputs:

1. `name` — the optional user-facing thread title. A Codex rename emits
   `thread/name/updated`, which Switchboard applies immediately.
2. `preview` — usually the first user message. When `name` is empty,
   Switchboard deterministically extracts up to five useful words / 40 runes
   from the first useful sentence and renders a lowercase hyphenated task name.
   For example, “I'd like you to help me with Codex session naming” becomes
   `codex-session-naming`.

The explicit Codex name always wins. Preview derivation is a display-only
fallback: Switchboard does not call `thread/name/set`, rewrite Codex storage, or
feed the derived value back into the live TUI. If app-server metadata is not
available, a Codex chip falls back to the project/cwd name and then `pid N` —
never to the moving terminal title or UUID.

This separation keeps naming independent from status. App-server runtime and
attention notifications continue to paint the chip color; a name or spinner
change cannot turn a chip green, orange, or red.

## Components

- **`internal/projectname`** — the pure resolver: prefix + dedup (longest-alias,
  hyphen-boundary), git-root detection, and a writable user config layered over
  built-in defaults.
- **`switchboard-ctl name`** — `resolve --cwd --name` (used by the wrapper),
  `abbrev --cwd` (current abbreviation), `set <dir> <abbrev>` (persist).
- **`internal/label`** — sources Claude names from
  `~/.claude/sessions/<pid>.json` (with its existing terminal-title fallback)
  and Codex names from the app-server root node (never its terminal title), then
  applies the project prefix. Shared by the Waybar chips and
  `switchboard-ctl pick`/`list`.
- **`internal/provider/codex`** — reads `Thread.name`/`Thread.preview`, derives
  the bounded unnamed-thread fallback, and consumes `thread/name/updated`.
- **launcher wrapper** — `~/.config/scripts/claude-name-wrapper.sh`.
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
