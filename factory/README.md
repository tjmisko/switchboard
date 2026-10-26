# Factory

A daily watch on the three agent CLIs switchboard observes: Claude Code, Codex
and Pi. They ship several times a week, have no stable internal contracts, and
any release can silently change what a session's status means. The factory
records what changed and flags what touches us. It never changes switchboard.

| here (code) | purpose |
|---|---|
| [surface.toml](surface.toml) | what we depend on, its risk, fingerprint patterns, changelog sources and keywords |
| [run](run) | `sweep`, `report`, `mark` |
| [systemd/](systemd) | daily timer |

| `~/.local/share/switchboard-factory` (data, its own git repo) | |
|---|---|
| `fingerprints/<tool>.txt` | the installed tool's hook events, app-server protocol, extension events, CLI flags, doc hashes; one line each |
| `changes.jsonl` | one record per change: fingerprint lines matching a surface, and every changelog bullet |
| `state.json` | per-tool installed version and changelog floor, last run's errors |

## A sweep

`factory/run` (what the timer runs):

1. Fingerprints each installed tool. Changed lines that match a surface's
   `fingerprint` patterns become records.
2. Reads each changelog (Claude and Pi `CHANGELOG.md`, Codex GitHub releases)
   and records every bullet at or above the tool's floor that isn't recorded
   yet. This covers versions that were never installed, and versions released
   upstream that you haven't installed yet (`ahead`).
3. Classifies new records as `threat` / `depends` / `new` / `ignore` with
   `claude -p` against surface.toml. The call has no tools, no settings (so no
   hooks reach switchboard), no MCP and no saved session.
4. Commits the data repo. The subject is the status line:

   ```
   sweep 2026-11-02: claude 2.1.310->2.1.318 codex 0.163.0->0.165.0 pi 1.0.4 | +412 (2 threat, 9 new, 31 depends)
   ```

   The body lists each threat. A sweep that finds nothing makes no commit, so
   `git -C ~/.local/share/switchboard-factory log --oneline` is the change history.

Missed days cost nothing. Every step is keyed by version and record id, not
by date, so one run after a week offline catches up completely, and running
it twice adds nothing. A failed fetch exits non-zero; the unit retries every
30 minutes, and anything still missing is picked up the next day.

## Triage

```bash
factory/run report         # open threats, unclassified, new
factory/run report --all   # everything
factory/run mark <id-prefix> threat "check ThreadActiveFlag mapping"
factory/run mark <id-prefix> done
```

`depends` and `ignore` are closed when classified; `threat` and `new` stay
open until marked `done`. Your `mark` overrides the model's class and is
committed. Threats that need code become switchboard issues; put the issue in
the note.

## Install

The unit runs `~/Projects/switchboard/factory/run`, so install it from main:

```bash
install -Dm644 factory/systemd/switchboard-factory.{service,timer} -t ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now switchboard-factory.timer
journalctl --user -u switchboard-factory   # run logs
```

## Knobs

- `state.json` → `tools.<tool>.floor`: lowest changelog version to record.
  Set on the first sweep to the installed version; lower it to backfill.
- `CLAUDE_BIN`, `CODEX_BIN`, `PI_PKG`: fingerprint an older install still on
  disk (`~/.local/share/claude/versions/<ver>`,
  `~/.codex/packages/standalone/releases/<ver>-*/bin/codex`).
- `FACTORY_MODEL` (default `sonnet`); `--no-classify`, `--no-commit`.
- `SWITCHBOARD_FACTORY_DATA` moves the data repo.

## Limits

- The Claude fingerprint sees hook event *names* only. Payload fields come from
  minified construction sites; re-verify them by hand per
  `docs/claude-code-hook-schema.md`.
- Transcript, rollout and session-file layouts are not fingerprinted; no tool
  publishes a schema for them. The changelog and test failures are the only
  signals.
- Usage-limit message text can change without a changelog line.
- Changelog bullets are untrusted text fed to a model. With no tools the worst
  outcome is a wrong class, which is why `threat` wins ties.
- Pi's `[Unreleased]` section is skipped; its bullets arrive with the release.
