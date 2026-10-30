# Session names, project directories, and display styling

Every provider uses the same visible convention:

**Project or alias** Task Title With Spaces

The project is bold at the normal bar size. Task text uses 90% size with a
half-point upward offset to keep it vertically centered beside the project.
The task uses title case, spaces instead of dashed
separators, and at most five words. An unnamed session shows only its project.
The directory's spelling, case, and Unicode characters are preserved unless a
project alias is configured.

| Directory / alias | Native task name | Bottom chip |
| --- | --- | --- |
| `中国地理` | `map-update` | **中国地理** Map Update |
| `MyProject` | none | **MyProject** |
| Switchboard alias `sb` | `sb-fix-project-name-detection-in-all-providers` | **sb** Fix Project Name Detection In |
| Arachne alias `ar` | `Refactor auth module` | **ar** Refactor Auth Module |

## Integration map

| Provider | Authoritative task input | Transport / implementation | Visible fallback |
| --- | --- | --- | --- |
| Claude | `/name` or `/rename`, including `claude -n` | `~/.claude/sessions/<pid>.json`, read by `internal/label` | Normalized terminal title, then project alone |
| Codex | Conversation-bound Switchboard `display_name`, then native app-server root name | `internal/provider/codex`, daemon naming hooks and read-only observer | Project alone; terminal titles never supply task text |
| Pi | Native `/name` for the current Pi session | `integrations/pi/switchboard.ts` forwards `session_start` / `session_info_changed` → `switchboard-ctl pi-hook` → native `display_name` | Project alone; never Claude files or Pi terminal chrome |

Previously, `internal/projectname` supplied a dashed project prefix and
`internal/label.RawName` shortened Codex tasks to three words and Pi tasks to
four. Claude names were largely verbatim. Pi without a native name could reach
Claude's terminal fallback and display `π - directory` as a task. Project
discovery already walked Git roots and supported aliases, but had no
configurable project parent directories.

Now `internal/label.DisplayName` selects unshortened native task text and
`internal/projectname.ProjectRule.TaskTitle` normalizes it once, after removing
the project prefix. This avoids consuming the task's five-word budget with a
prefix or applying provider-specific limits first. Source names and persisted
records remain unchanged.

The bottom-bar broker and standalone `switchboard-waybar` share
`internal/waybarchip.Renderer`. It fits plain display text against the bar width,
then escapes user text and applies Pango `<b>` only to the project portion.
The display publisher, picker, and Polybar use the same plain display label;
Waybar's hover card also uses the shared normalized task. Compact circles keep
their existing geometry and shared hover card.

## Project resolution and configuration

Settings live in `$XDG_CONFIG_HOME/switchboard/projects.json`, defaulting to
`~/.config/switchboard/projects.json`:

```json
{
  "project_dirs": ["~/Projects", "~/Tools"],
  "projects": {
    "/home/you/Projects/Arachne": {
      "canonical": "ar",
      "aliases": ["ar", "arachne"],
      "full": "Arachne"
    }
  }
}
```

`project_dirs` lists parent directories, not individual projects. Its default
is `~/Projects` and `~/Tools`. Absolute paths and a leading `~/` are supported;
other relative paths are ignored. An explicit empty array disables parent
matching. Existing project files without this field inherit the defaults.
Changing an alias preserves this setting.

Resolution is shared by Claude, Codex, Pi, the display, and CLI naming commands:

1. Inside a configured parent, use its immediate child as the project. For
   example, `~/Tools/MyTool/src/parser` belongs to `MyTool`, even without Git.
   A nested Git checkout or worktree stays grouped under that project. If
   parents overlap, the most specific parent wins.
2. Else use the nearest ancestor with a `.git` directory or file.
3. Else use the current directory itself.
4. Apply a path-specific alias first, then a built-in basename rule. Otherwise
   display the project directory name verbatim.

Built-in aliases remain `arachne`, `sspi-data-webapp → sspi`,
`switchboard → sb`, and `switchboard-dashboard → sb-dash`. A `full` entry
controls the expanded project title in the hover card; it does not replace the
chip's alias. Unknown project names need no configuration.

Remote paths are never walked on the local filesystem or matched against local
path overrides. The current remote fallback is lexical basename resolution;
local project parent settings do not infer another machine's home directory.

```sh
switchboard-ctl name display --cwd ~/Projects/Arachne --name refactor-auth-module
# arachne Refactor Auth Module
switchboard-ctl name set ~/Projects/Arachne ar
switchboard-ctl name set-full --cwd ~/Projects/Arachne --name Arachne
```

Middle-clicking a chip opens the shared project alias editor. Its historical
script name, `~/.config/scripts/claude-abbrev-edit`, is retained for existing
bindings, but the alias applies to all providers in the project. Its source
lives in `linux-config/common/.config/scripts`, deployed through stow. Remote
aliases are edited on their source host. Renderer configuration reloads on the
next snapshot after the file's stamp changes.

## Dashed labels and identity

`switchboard-ctl name resolve --cwd <dir> --name <task>` still returns the
legacy project-prefixed label, such as `sb-fix-project-names`. `name abbrev`
returns its canonical project prefix. Unicode letters, numbers, and combining
marks survive sanitization. `internal/label.Chip` / `RawName` retain the
historical dashed-label APIs, including their compact provider fallbacks.

The optional Claude startup wrapper still uses `name resolve` to scope a
`claude -n` name. It does not own Switchboard's display styling. Pi and Codex do
not need launcher wrappers for project names.

A dashed label is not a unique session ID. Navigation and click targets remain
bound to the host, PID, and process start time. Changing display text, aliases,
or casing does not change those selectors. `display.json`'s `label` is plain
readable text; its `key` is the stable session identity. Pango belongs only in
markup fields and Waybar's module text, never in an identity or stored name.

## Native names and generation

Switchboard does not rewrite providers' live names. Claude owns its in-memory
name; external writes to its transcript or session file do not change its live
border. The startup wrapper can only affect launch-time `-n` values.

Codex's first usable completed turn can generate a Switchboard-owned name.
The naming call receives the cwd basename plus bounded prompt and final
response excerpts (1,000 Unicode characters each). Its output remains a 2–5
word kebab-case value capped at 40 characters, validated with one retry and a
deterministic fallback. Only the conversation-bound display record persists;
prompt/response excerpts and pending attempts do not enter state or history.
The naming model is configurable with `-codex-autoname-model`.

An authoritative Codex native rename clears the generated record when it
differs from the recorded baseline. Pi forwards its native name verbatim and
binds it to its Pi session ID. `/new` or `/resume` invalidates stale records;
renaming carries no status evidence and does not renew a hook lease. Display
normalization never mutates either provider's native value.

## Styling guidance and activation

- Keep project and task separate until rendering. Bold only the project or
  alias, preserving its configured spelling; title-case and cap only the task.
- Strip a known project prefix before counting task words. Accept both the
  legacy dashed prefix and readable words; do not introduce provider names or
  glyphs as tasks.
- Fit and truncate plain text before escaping and adding markup. Preserve
  Unicode and escape all user-controlled markup characters.
- Keep status colors, focus borders, remote borders, and navigation independent
  of naming. Tooltips retain full project/path context and coarse clocks.
- Reuse the shared resolver and formatter in integrations instead of copying
  per-provider naming rules into shell or TypeScript.

Deploy a managed installation with `scripts/deploy`; it installs the matching
daemon, control client, and renderer and verifies the running revision. Do not
use `go install` to update an installation managed through `current`. Pi's
extension should remain linked to `integrations/pi/switchboard.ts`; stow owns
the shell scripts in `linux-config`.
