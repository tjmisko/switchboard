## Phase 4.3 — The macOS Navigate Matrix (Window Managers + Terminals)

One line: catalog which `wm.Manager` and `terminal.Locator` backends are realistic on macOS, how detection picks them, and what to actually ship so Navigate works on a Mac the way it does on Linux.

> **Status: design/scoping — Observe works once 4.1/4.2 land; Navigate is opt-in per WM.**

The two tiers carry over unchanged from Linux:

- **Observe** (count / cwd / status) rides only the OS process layer. Once Phase 4.1 (process inspection) and 4.2 (cwd/tty resolution) land for Darwin, Observe works on *any* Mac with zero WM or terminal backend. There is nothing macOS-specific to gate here beyond the syscalls those phases provide.
- **Navigate** (focus a session's window + pane) needs BOTH a `wm.Manager` backend (to raise/focus the window) AND a `terminal.Locator` backend (to select the pane inside it). `rpc.focus` only returns `ErrNavigateUnsupported` when *both* resolve to `none`. So the bar for "Navigate works on macOS" is: at least one viable WM backend, paired with a locator that is already present.

The good news, established below: the terminal half is essentially free (tmux and wezterm already drive their CLIs cross-platform), so the entire macOS Navigate effort collapses onto **shipping one WM backend**. AeroSpace is the recommendation.

---

### 1. Terminal backends on macOS

The `terminal.Locator` contract is `Name()/Available()/Locate(ctx, tty)/Activate(ctx, *PaneRef)`, with `tty` (here `/dev/ttysNNN`) as the portable join key and a `Chain` that tries locators innermost-first and routes `Activate` by `PaneRef.Backend`. Two existing backends are OS-agnostic because they shell out to a CLI rather than touch Linux internals:

- **tmux** — drives the `tmux` CLI (`tmux list-panes -a -F …`, `tmux select-window`/`select-pane`/`switch-client`). The CLI is identical on macOS. `Locate(tty)` matches against `#{pane_tty}`; `Activate` selects the pane. **Works today, zero new code.** This is the keystone: tmux gives pane-level Navigate independent of which terminal *emulator* hosts the tmux client, which sidesteps the per-emulator automation problem entirely.
- **wezterm** — drives `wezterm cli list --format json` and `wezterm cli activate-pane --pane-id`. The CLI ships in the macOS app bundle. `Locate(tty)` matches the `tty_name` field. **Works today if wezterm is installed, zero new code.**

The hard cases are the native/closed emulators, which expose no pure-CLI pane addressing:

- **iTerm2** — has a real automation surface (the Python API and AppleScript), and the community `it2` CLI wraps it. Sessions carry a `tty` property (`/dev/ttysNN`) and `async_activate(select_tab, order_window_front)` does exactly the focus-the-pane operation we need. So a future `terminal/iterm2.go` is *feasible*: `Locate` iterates app→window→tab→session matching `session.tty == tty`, returns a `PaneRef{Backend:"iterm2", Address:session_id}`; `Activate` calls `async_activate`. The friction is the transport — it is not a stable single-binary CLI like `tmux`/`wezterm cli`. Options: (a) shell out to `it2` if present (third-party, must be installed and kept in sync), (b) spawn a short-lived Python helper against the bundled `iterm2` module (requires the Python API enabled in iTerm2 prefs + the runtime), or (c) AppleScript `tell application "iTerm2"` which can read `tty` per session but addressing is clunky. All add a process spawn per locate. **Defer to a follow-up; tmux-inside-iTerm2 already covers the common case.**
- **Terminal.app** — AppleScript only (`tell application "Terminal"`). It exposes windows/tabs and a `tty` per tab, so detection and *reading* a session's tty is possible, but it has **no pane concept and weak programmatic tab activation** (you can set `selected tab`, but it is fragile and slow). Realistically **Observe-only**; for Navigate, the answer is "run tmux inside it."
- **kitty** — `kitten @ ls` / `kitten @ focus-window` work *only* when remote control is enabled and `KITTY_LISTEN_ON` is exported. Same posture as on Linux: a clean future backend, but **deferred** — gated behind the `KITTY_LISTEN_ON` socket signal.

| Terminal | Navigate support | Detection signal | Effort |
|---|---|---|---|
| tmux | Yes — pane-level, today | `tmux` on PATH + `$TMUX` set (inside) / server socket | none (ships) |
| wezterm | Yes — pane-level, today | `$WEZTERM_PANE` / `$WEZTERM_UNIX_SOCKET`, `wezterm` on PATH | none (ships) |
| iTerm2 | Possible (Python API / `it2` / AppleScript) | `$ITERM_SESSION_ID`, `$TERM_PROGRAM=iTerm.app` | M — new `terminal/iterm2.go`, spawns helper |
| Terminal.app | Observe-only (AppleScript, no panes) | `$TERM_PROGRAM=Apple_Terminal` | not worth it (use tmux) |
| kitty | Possible, deferred | `$KITTY_LISTEN_ON` present | M — `terminal/kitty.go`, gated on socket |

---

### 2. WM backends on macOS

There is no EWMH/i3-IPC equivalent baked into macOS, so each candidate is a third-party tool with its own CLI/socket. Mapping onto the `wm.Manager` contract — `Name()/Available()/Clients()/ActiveWindow()/Focus()/Subscribe()`, `Window{Address,PID,Title,Workspace,WorkspaceID}`, neutral events `focus-changed`/`window-closed`/`layout-changed`:

#### yabai
Scriptable tiling WM with a mature query/command CLI over a unix socket.
- **Clients** — `yabai -m query --windows` returns JSON with `id`, `pid`, `app`, `title`, `space`, `has-focus`, plus `is-visible`/`is-minimized`/etc. Maps cleanly: `Address=id` (stringified), `PID=pid`, `Title=title`, `WorkspaceID=space`.
- **ActiveWindow** — `yabai -m query --windows --window` (focused window), or filter `has-focus==true`.
- **Focus** — `yabai -m window --focus <id>`.
- **Events** — first-class signals: `yabai -m signal --add event=window_focused action=…` (also `window_created`/`window_destroyed`/`space_changed`). These map to `focus-changed`/`window-closed`/`layout-changed`. `Subscribe` can register a signal that pipes events back to switchboard (e.g. via a helper that writes to a socket/fifo).
- **Friction (call out loudly):** yabai needs **partial SIP disabled** + a **scripting addition injected into Dock.app** for the privileged window-server operations. Query and basic focus work without it on recent macOS, but reliability of focusing/moving windows degrades, and the scripting addition must be reloaded after Dock restarts and re-signed across OS updates. High setup burden; not something we can assume.

#### AeroSpace — recommended
Newer i3-like tiling WM, explicitly **SIP-free** (uses public Accessibility APIs), with a first-class CLI.
- **Clients** — `aerospace list-windows --all --json` (or `--monitor`/`--workspace` scoped), `--format` exposes `window-id`, `app-name`, `window-title`, `app-pid`, `workspace`. Maps to `Address=window-id`, `PID=app-pid`, `Title=window-title`, `Workspace=workspace`.
- **ActiveWindow** — `aerospace list-windows --focused --json`.
- **Focus** — `aerospace focus --window-id <id>`.
- **Events** — **no streaming event API.** AeroSpace offers config-level callbacks (`on-focus-changed`, `exec-on-workspace-change`) that fire a command, not a subscribable stream switchboard can consume directly. `Subscribe` therefore must be **poll-based** (periodic `list-windows --focused` diff) or simply rely on the reconcile tick. This is the main contract gap and it is acceptable — see Risks.
- **Friction:** minimal. No SIP, no scripting addition, single static CLI binary. This is why it is the recommendation.

#### Default macOS (no tiling WM)
The stock WM has **no IPC/EWMH**. Programmatic window focus requires the **Accessibility API** (`AXUIElement`, needs the Accessibility permission granted to the switchboard binary) or AppleScript `System Events`. Both are heavy, permission-gated, and have no clean per-window stable address. **Stock macOS is Observe-only.** Stated plainly: on a Mac with no tiling WM, switchboard reports counts/cwd/status but `rpc.focus` raises `ErrNavigateUnsupported` for the WM half (terminal-only focus may still select the pane via tmux/wezterm, but the window cannot be reliably raised).

| WM | Clients | Focus | Events (Subscribe) | Detection | Opaque ref | Effort / risk |
|---|---|---|---|---|---|---|
| AeroSpace | Yes (`list-windows --json`) | Yes (`focus --window-id`) | Poll only (callbacks fire commands, no stream) | `aerospace` on PATH + its socket | AeroSpace `window-id` (int→string) | **Low** — recommended |
| yabai | Yes (`query --windows`) | Yes (`window --focus`) | Yes (`signal`) | `yabai` on PATH + `/tmp/yabai_$USER.socket` | yabai window `id` (int→string) | Med–High — SIP + scripting addition |
| stock macOS | AX API only | AX/AppleScript, fragile | No | always (fallback) | none | Observe-only |

---

### 3. Recommended scope for Phase 4.3

1. **Observe everywhere, free.** Once 4.1/4.2 land, Observe works on any Mac with no WM/terminal backend at all. Ship and document this as the baseline.
2. **Terminal Navigate is already free.** tmux works today (OS-agnostic CLI); wezterm works today if installed. The `Chain` composes them innermost-first. No macOS terminal code is required for the common workflow (tmux-in-any-terminal).
3. **Ship AeroSpace as the WM backend.** Cleanest fit: SIP-free, static CLI, exposes `window-id`, `app-pid`, `workspace`, `--focused`, and `focus --window-id` — a near 1:1 map onto the `wm.Manager` contract. The only gap is `Subscribe`, handled by polling/reconcile.
4. **yabai as a second, opt-in option.** Strictly better events (signals → real `Subscribe`) but gated on SIP + scripting-addition friction we cannot assume. Implement if/when there is demand; same `Window` mapping, different transport.
5. **Stock macOS stays Observe-only.** Document, do not engineer the AX path now.

Net: **AeroSpace + tmux is the recommended Navigate combo on macOS**, and tmux/wezterm already deliver the pane half for free. The whole 4.3 Navigate deliverable is one backend file (`wm/aerospace.go`) plus its conformance adoption.

---

### 4. The macOS PID-join reality

The portable join is `(mux pid, title)` → WM window, exactly as on Linux/i3. The question is whether each candidate WM exposes the window's **owning pid**:

- **yabai** — `pid` is a top-level field in `query --windows`. ✓
- **AeroSpace** — `app-pid` (the owning application's UNIX pid) is an available `--format` variable. ✓ Use `app-pid` to fill `Window.PID`.

So `(mux pid, title)` resolves on both. The same Linux caveat applies for **tmux-inside-a-terminal**: tmux is a server process; the pid the WM reports is the *terminal emulator* hosting the tmux client, not tmux itself. The tmux↔WM bridge therefore matches on the emulator's window (via its pid/title) and then tmux selects the pane within — identical two-stage handoff to Linux, no new macOS wrinkle. The tty remains the authoritative join key into the terminal locator; the WM pid join only has to get focus to the right *window*.

---

### 5. Detection precedence on macOS

`internal/detect` stays cheap and side-effect-free (env vars + socket existence), picks backends at runtime, and reports the `capabilities` block. WM auto-detect is an ordered env/socket probe:

**WM precedence (first match wins):**
1. **AeroSpace** — `aerospace` on PATH and/or its running socket. Preferred because it is friction-free and most likely to actually work without elevated setup.
2. **yabai** — `yabai` on PATH **and** `/tmp/yabai_$USER.socket` exists (socket existence implies a running server; PATH alone is not enough since the scripting addition may be absent).
3. **none** — stock macOS; Observe-only, `Focus` unsupported.

(If a user runs both, AeroSpace winning is the safe default since it needs no privileged setup; allow an override via the same env knob Linux uses for WM precedence.)

**Terminal precedence** is already handled by the existing `Chain` and needs no macOS-specific ordering: probe innermost-first — `$TMUX` (tmux) → `$WEZTERM_PANE`/`$WEZTERM_UNIX_SOCKET` (wezterm) → `none`. Because tmux is the innermost mux, the Chain naturally locates the tmux pane first and only falls back to the emulator. iTerm2/kitty, when implemented, slot in as additional emulator-level locators behind the same `$TERM_PROGRAM` / `$KITTY_LISTEN_ON` signals.

The reported `capabilities` block on macOS should therefore read, e.g.: `{ observe: true, navigate: <wm != none && terminal != none>, wm: "aerospace"|"yabai"|"none", terminal: "tmux"|"wezterm"|"none" }`.

---

### 6. Definition of Done (mirrors plan 4.3)

- [ ] macOS runs the **Observe tier out of the box** — count / cwd / status with no WM or terminal backend, on any Mac, once 4.1/4.2 land.
- [ ] This **Navigate matrix is documented** (the two tables above): which terminals and WMs support Navigate, their detection signals, and effort.
- [ ] Detection reports a correct `capabilities` block on macOS (wm + terminal + observe/navigate booleans).
- [ ] **(Stretch)** At least one WM backend ships — **AeroSpace recommended** — and, paired with the already-working **tmux** locator, delivers end-to-end Navigate (`rpc.focus` raises the AeroSpace window and tmux selects the pane) on a Mac.
- [ ] The shipped WM backend adopts `conformance.RunManagerContract`: event-name translation + address-normalization round-trip run always; live client/focus assertions gate on `Available()` + `SWITCHBOARD_LIVE_CONFORMANCE=1`.

---

### 7. Risks

- **SIP / scripting-addition friction (yabai).** Full yabai control needs partial SIP disable + a Dock.app scripting addition that must be reloaded on Dock restart and re-signed across macOS updates. We cannot assume it is present; this is the core reason AeroSpace is preferred. Detection must key on the *socket existing*, not just the binary on PATH, to avoid claiming Navigate when the addition is missing.
- **Accessibility permission (AeroSpace + any AX path).** AeroSpace itself relies on the Accessibility permission being granted to *AeroSpace*; switchboard does not need its own grant since it only drives the `aerospace` CLI. Avoid building our own AX-based focus for stock macOS — it would force switchboard into the permission-grant flow.
- **No event stream from AeroSpace.** `Subscribe` cannot deliver real-time `focus-changed` from AeroSpace; the callbacks fire shell commands rather than expose a stream. Mitigation: implement `Subscribe` as a low-frequency poll of `list-windows --focused`, or — preferred — simply rely on the existing reconcile tick and have `Subscribe` return a closed/empty channel when the backend has no native stream. yabai (signals) is the only macOS WM that can back a true event stream.
- **AppleScript / helper-spawn latency.** Any iTerm2/Terminal.app path pays a per-call process spawn (AppleScript or a Python helper), measurable latency vs. a socket CLI. Another reason to lean on tmux/wezterm and treat native-emulator locators as opt-in extras.
- **AeroSpace is pre-1.0.** Public beta; CLI flags and JSON fields may shift before 1.0. Pin the `--format` field names we depend on (`window-id`, `app-pid`, `app-name`, `window-title`, `workspace`) and add a version probe so a breaking change degrades to Observe rather than mis-focusing.
- **tmux-vs-emulator pid ambiguity.** As in Linux, the WM-reported pid for a tmux session is the emulator's, not tmux's. The two-stage join (WM window by emulator pid/title, then tmux pane by tty) is correct but means the WM-side match must tolerate the emulator owning multiple tmux sessions — disambiguation happens in the terminal locator via tty, not in the WM.
