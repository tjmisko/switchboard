# Machine-specific configuration

Switchboard's daemon is portable; desktop activation is not. A Hyprland laptop
can use the Waybar appliance while an i3 workstation uses the standalone
Polybar, even when both machines pull the same repository and dotfiles.

Keep configuration in three layers:

1. **Shared runtime:** `systemd/switchboard.service` and the Switchboard
   binaries. This layer detects the local WM and terminal at runtime.
2. **Integration assets:** `polybar/switchboard.ini`, the Waybar appliance, and
   the two renderer units. Installing an asset makes it available; it does not
   select it.
3. **Host activation:** systemd drop-ins, enabled renderer unit, and WM startup
   lines. This is the only layer that chooses Waybar or Polybar and belongs to
   the machine, not the shared profile.

## Rules

- Activate exactly one Switchboard renderer on a graphical host. The reference
  TUI can coexist with either renderer.
- Treat the checked-in bar files as named integration assets, never as a
  machine-independent "current bar". Adding Polybar must not remove, overwrite,
  or auto-disable Waybar.
- Keep host values in a systemd drop-in. Do not copy and edit the shared
  `ExecStart`: doing so freezes its backend-discovery logic and makes later
  updates appear installed when an old drop-in is still winning.
- A renderer unit is opt-in. Do not enable a renderer from an install script or
  package post-install step.
- Import the graphical environment before starting the renderer. Waybar needs
  the active Wayland display; Polybar needs `DISPLAY` and `I3SOCK`.
- Install the daemon, control client, and renderer helpers from the same
  revision. A machine-local binary override is an executable deployment, not
  just a path preference; update the overridden binaries together before
  restarting their units.
- In a shared dotfiles repository, store overlays under a hostname directory
  (for example `hosts/goosebook/...`) and install only that host's overlay.
  Do not sync one host's enabled-unit symlinks as universal configuration.

## Shared daemon with host-local values

Install the shared unit unchanged:

```bash
install -Dm644 systemd/switchboard.service \
  ~/.config/systemd/user/switchboard.service
```

The unit exposes `SWITCHBOARD_BIN` and `SWITCHBOARD_ARGS`. A development host
can override them without replacing the service command:

```ini
# ~/.config/systemd/user/switchboard.service.d/20-machine.conf
[Service]
Environment=SWITCHBOARD_BIN=/home/alice/.config/switchboard/bin/switchboard
Environment="SWITCHBOARD_ARGS=-remote buildbox -remote user@gpu-box"
```

`SWITCHBOARD_ARGS` uses systemd's command-line word splitting, so quote any
individual argument containing whitespace inside the value. Most hosts need no
override at all.

## Remote command path for SSH federation

For every `-remote <destination>`, the client daemon starts a separate,
noninteractive command equivalent to:

```text
ssh -n -T <destination> switchboard-ctl remote-stream
```

The remote `switchboard-ctl` must therefore resolve by its bare name in the
login shell's **noninteractive SSH path**. An interactive shell finding the
binary is not sufficient. Neither is a remote systemd override such as
`SWITCHBOARD_BIN=/home/alice/.config/switchboard/bin/switchboard`: that setting
selects the daemon executable only and does not change the SSH command's path.

First inspect the actual command environment from the client host. The printed
path, not an already-open remote terminal, is authoritative:

```bash
ssh -n -T buildbox 'printf "%s\n" "$PATH"; command -v switchboard-ctl'
```

Do not assume that editing `~/.bashrc` changes this result. SSH servers, login
shells, and shell startup files differ; an interactive shell may add
`~/.local/bin` while an SSH command session does not read that setup at all.

When `~/.local/bin` is present in the printed path, a user-owned symlink is a
durable rootless installation. It keeps following future binary replacements at
the target path, unlike a copied helper. Run this once on the remote host; the
existing-path check deliberately refuses to overwrite an unrelated command:

```bash
(
    switchboard_ctl_target="$HOME/.config/switchboard/bin/switchboard-ctl"
    switchboard_ctl_link="$HOME/.local/bin/switchboard-ctl"

    if [ ! -x "$switchboard_ctl_target" ]; then
        echo "missing executable $switchboard_ctl_target" >&2
        exit 1
    fi
    install -d -m 0755 "$HOME/.local/bin"
    if [ ! -e "$switchboard_ctl_link" ] && [ ! -L "$switchboard_ctl_link" ]; then
        ln -sT "$switchboard_ctl_target" "$switchboard_ctl_link"
    elif [ "$(readlink -f -- "$switchboard_ctl_link")" != \
           "$(readlink -f -- "$switchboard_ctl_target")" ]; then
        echo "refusing to replace existing $switchboard_ctl_link" >&2
        exit 1
    fi
)
```

If `~/.local/bin` is absent, place the command in a directory which is actually
present in the printed path. `/usr/local/bin` is the usual administrator-owned
choice on a personal development host. After confirming that directory appears
in the probe, run this on the remote host; `ln` has no force option here and
therefore refuses to replace an existing command:

```bash
(
    switchboard_ctl_target="$HOME/.config/switchboard/bin/switchboard-ctl"
    if [ ! -x "$switchboard_ctl_target" ]; then
        echo "missing executable $switchboard_ctl_target" >&2
        exit 1
    fi
    sudo ln -sT -- "$switchboard_ctl_target" /usr/local/bin/switchboard-ctl
)
```

The system-path symlink deliberately delegates that command name to the owning
user's development binary. On a multi-user host, prefer a root-owned packaged
copy and update it as part of every Switchboard deployment instead. In either
case, verify from the client host:

```bash
ssh -n -T buildbox 'command -v switchboard-ctl'

timeout 5s ssh -n -T \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=yes \
    buildbox switchboard-ctl remote-stream >/dev/null
test "$?" -eq 124
```

Exit 124 is expected in the second probe: `timeout` stopped a healthy,
long-lived stream. Exit 127 means the remote command is still absent from the
noninteractive path; exit 255 points instead to SSH resolution, host-key, or
batch-authentication failure. Exit 2 accompanied by usage text which omits
`remote-stream` means the remote `switchboard-ctl` is older than the federation
feature or otherwise mismatched with the daemon.

For a private development install, build both remote binaries from the same
checkout and replace their stable targets atomically. Staging inside the target
directory keeps each final rename on one filesystem, so existing symlinks remain
valid and no running executable is truncated in place:

```bash
(
    set -eu
    switchboard_repo="$HOME/Projects/switchboard"
    switchboard_bin_dir="$HOME/.config/switchboard/bin"

    if [ ! -f "$switchboard_repo/cmd/switchboard-ctl/remote_stream.go" ]; then
        echo "checkout predates SSH federation: $switchboard_repo" >&2
        exit 1
    fi

    install -d -m 0755 "$switchboard_bin_dir"
    switchboard_stage="$(mktemp -d "$switchboard_bin_dir/.deploy.XXXXXX")"
    cd "$switchboard_repo"
    go build -o "$switchboard_stage/switchboard" ./cmd/switchboard
    go build -o "$switchboard_stage/switchboard-ctl" ./cmd/switchboard-ctl
    chmod 0755 "$switchboard_stage/switchboard" \
        "$switchboard_stage/switchboard-ctl"
    mv -T "$switchboard_stage/switchboard" "$switchboard_bin_dir/switchboard"
    mv -T "$switchboard_stage/switchboard-ctl" \
        "$switchboard_bin_dir/switchboard-ctl"
    rmdir "$switchboard_stage"
)
```

Restart the remote daemon after both renames:

```bash
systemctl --user daemon-reload
systemctl --user restart switchboard.service
```

The client-side SSH worker needs no restart: its next retry executes the new
control binary. Before a first frame arrives, the client journal shows repeated
entries such as
`remote-state: destination=<host> host= category=disconnected`. Once the command
resolves, its two-second retry loop reconnects automatically.

## Hyprland + Waybar host

Install the Waybar renderer unit and enable it only on the Waybar host:

```bash
install -Dm644 systemd/switchboard-waybar.service \
  ~/.config/systemd/user/switchboard-waybar.service
systemctl --user daemon-reload
systemctl --user enable switchboard-waybar.service
```

The normal top Waybar remains owned by Hyprland. The renderer unit owns only
the auto-hiding Switchboard bottom process, replacing an unmonitored
`switchboard-ctl bottombar watch` child. A Hyprland startup should import its
environment and start the graphical session target before launching the top
bar:

```text
systemctl --user import-environment HYPRLAND_INSTANCE_SIGNATURE WAYLAND_DISPLAY XDG_CURRENT_DESKTOP DISPLAY
systemctl --user start graphical-session.target
waybar
```

If this host runs a development `switchboard-ctl`, override only the value:

```ini
# ~/.config/systemd/user/switchboard-waybar.service.d/20-machine.conf
[Service]
Environment=SWITCHBOARD_CTL=/home/alice/.config/switchboard/bin/switchboard-ctl
```

Build that helper alongside the daemon selected by `SWITCHBOARD_BIN`. The
bottom-bar watcher falls back to the legacy local-only RPC during a rolling
upgrade, but mixed revisions should be temporary and visible in its journal.

Do not also launch `switchboard-ctl bottombar watch` from Hyprland once the
unit owns it.

## i3 + Polybar host

Install the Polybar asset and renderer unit only on the Polybar host:

```bash
install -Dm644 polybar/switchboard.ini \
  ~/.config/polybar/switchboard.ini
install -Dm644 systemd/switchboard-polybar.service \
  ~/.config/systemd/user/switchboard-polybar.service
systemctl --user daemon-reload
systemctl --user enable switchboard-polybar.service
```

i3 must import its live values before bringing up the graphical session target:

```text
systemctl --user import-environment DISPLAY XAUTHORITY I3SOCK
systemctl --user start graphical-session.target
```

Do not also launch `polybar -c ... switchboard` from i3 once the unit owns it.

## Deployed layout on goosebook

This host runs a machine-local binary deployment. Recorded 2026-08-27 at
`18c9ba5`.

| What | Path | Read by |
| --- | --- | --- |
| Daemon (live) | `~/.config/switchboard/bin/switchboard` | `switchboard.service`, via `SWITCHBOARD_BIN` |
| Control client (live) | `~/.config/switchboard/bin/switchboard-ctl` | `switchboard-waybar.service`, via `SWITCHBOARD_CTL` |
| `go install` output | `~/go/bin/switchboard{,-ctl,-waybar,-codex}` | Claude Code hooks only — **not** the units |
| Previous binaries | `~/.config/switchboard/bin/.switchboard-backup-<date>-<tag>/` | nothing; kept for rollback |

Active units are `switchboard.service` and `switchboard-waybar.service`, both
enabled. `switchboard-polybar.service` is not installed: this is a Hyprland +
Waybar host.

The two overrides live in drop-ins, as the rules above require:

```ini
# ~/.config/systemd/user/switchboard.service.d/local-binary.conf
[Service]
Environment=SWITCHBOARD_BIN=%h/.config/switchboard/bin/switchboard
```

```ini
# ~/.config/systemd/user/switchboard-waybar.service.d/local-binary.conf
[Service]
Environment=SWITCHBOARD_CTL=%h/.config/switchboard/bin/switchboard-ctl
```

`switchboard.service.d/lock-debug.conf` also sets `SWITCHBOARD_DEBUG_LOCK=5ms`.
That one is measurement instrumentation, not a setting; drop it when the
store-lock arms are finished.

### `go install` alone does not redeploy this host

The shared unit defaults `SWITCHBOARD_BIN` to `%h/go/bin/switchboard`, and the
drop-in above overrides it. So `go install ./cmd/...` updates the hook binaries
and leaves the daemon untouched. Restarting afterward is a silent no-op: the
unit reports `active`, the journal looks healthy, and the old binary keeps
running. Nothing in the restart output distinguishes this from a real deploy.

Redeploy with the staged build in
[Remote command path for SSH federation](#remote-command-path-for-ssh-federation)
above — it stages inside the target directory so each `mv -T` stays on one
filesystem and no running executable is truncated in place — then:

```bash
systemctl --user restart switchboard.service
systemctl --user restart switchboard-waybar.service
```

Verify against behavior, not unit state. Keep the previous binaries and diff
their rendered output against the new ones:

```bash
~/.config/switchboard/bin/.switchboard-backup-<date>-<tag>/switchboard-ctl pick
~/.config/switchboard/bin/switchboard-ctl pick
```

Identical output where the commit claimed a label change means the deploy did
not land.

### Local edit to the installed unit

`~/.config/systemd/user/switchboard.service` is **not** identical to
`systemd/switchboard.service` in the repository. One line was edited in place to
add this host's federation peer:

```ini
Environment="SWITCHBOARD_ARGS=-remote nlessfun"   # repo ships SWITCHBOARD_ARGS=
```

Reinstalling the shared unit with `install -Dm644` silently reverts that and
drops the `nlessfun` peer. Move it into a `20-machine.conf` drop-in, or re-add
it after every unit reinstall. The peer resolves `switchboard-ctl` at
`/usr/local/bin/switchboard-ctl` on the remote's noninteractive SSH path.

## Switching a host

Switching renderers is an explicit host operation: disable the old renderer,
enable the new one, update the WM startup, then reload the user manager. Pulling
Git changes alone never changes the selected renderer. Check the effective
configuration with:

```bash
systemctl --user cat switchboard.service
systemctl --user is-enabled switchboard-waybar.service switchboard-polybar.service
systemctl --user status switchboard.service switchboard-waybar.service switchboard-polybar.service
```

The daemon and renderer are separate failure domains. A running
`switchboard.service` with no visible bar usually means the selected renderer
unit is inactive or lacks the graphical environment, not that discovery has
failed.
