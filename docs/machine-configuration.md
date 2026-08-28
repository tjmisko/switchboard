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
Environment=SWITCHBOARD_BIN=/home/alice/.local/share/switchboard/current/switchboard
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
`SWITCHBOARD_BIN=/home/alice/.local/share/switchboard/current/switchboard`: that setting
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
    switchboard_ctl_target="$HOME/.local/share/switchboard/current/switchboard-ctl"
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
    switchboard_ctl_target="$HOME/.local/share/switchboard/current/switchboard-ctl"
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

On the remote host, deploy the same way as anywhere else — from a checkout of
this repository:

```bash
cd ~/Projects/switchboard && scripts/deploy
```

That builds the daemon and the control client from one revision, stages them so
each rename stays on one filesystem, flips `current`, restarts the unit and then
verifies the running process. See [Deploying a release](#deploying-a-release).

Deploying both halves from one revision matters here specifically: a
`switchboard-ctl` older than the daemon is what produces the exit-2 usage output
described above.

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
Environment=SWITCHBOARD_CTL=/home/alice/.local/share/switchboard/current/switchboard-ctl
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

## Deploying a release

A deployment is one immutable directory per revision plus a `current` symlink:

```text
~/.local/share/switchboard/
  releases/42485f2/{switchboard,switchboard-ctl,switchboard-waybar}
  releases/2f60aef/…
  current -> releases/42485f2
```

Everything that runs a Switchboard binary resolves through `current`, so one
atomic rename moves the whole set together and a rollback is the same rename
backwards. Nothing is written in place, so no running executable is truncated
and the daemon and its control client can never be left as a half-updated pair.

Deploy with `scripts/deploy`. It builds every command with the revision stamped
in, installs the shared units and this host's overlay, flips `current`, restarts
the units, and then **verifies the running process** before reporting success:

```bash
scripts/deploy            # build, install, flip, restart, verify
scripts/deploy --dry-run  # print every action, change nothing
scripts/deploy --status   # what is deployed, running, and linked
scripts/deploy --rollback # flip to the previous release and restart
```

### Why the script verifies instead of trusting the restart

`systemctl restart` reports `active` whether or not new code landed. A deploy
that quietly kept the old binary produces a healthy unit, a clean journal, and
no signal of any kind. Ordering a restart is therefore not evidence, and the
script treats it as such — it asks the live process what it is:

```bash
pid=$(systemctl --user show switchboard.service -p MainPID --value)
"$(readlink -f "/proc/$pid/exe")" -version
```

Reading the answer from `/proc` means neither the unit file, the symlink, nor
the deploy's own expectations can launder it. When it disagrees with the
revision being shipped, the script flips back to the previous release and exits
non-zero. Four checks guard a deploy, in order:

1. **Wrong tree.** Refuses when the live revision is absent from the repository
   being built. More than one clone of this module can exist on a machine, with
   the same module path and the same command names, so building in the wrong
   directory replaces the right binaries at exit 0. A tree that cannot account
   for what is already deployed is the signature of exactly that.
2. **Downgrade.** Refuses when the new revision is an ancestor of the live one.
3. **Stamp.** Every staged binary must report the revision it was built with, so
   a broken `-ldflags` path is caught before anything goes live rather than
   silently disabling version reporting.
4. **Running process.** The check above.

Pass `--allow-downgrade` or `--allow-dirty` to override the first three
deliberately. Dirtiness is asked of `git status`, never read from the build
stamp: Go marks a *clean* linked worktree as modified, so under a
worktree-per-branch workflow that stamp is true almost always and cannot gate
anything.

### Stable command names

`scripts/deploy` publishes the commands into `~/.local/bin` as symlinks pointing
through `current`:

```text
~/.local/bin/switchboard-ctl -> ~/.local/share/switchboard/current/switchboard-ctl
```

Desktop configuration should reference **these** paths — a waybar module, a
Hyprland bind, a wezterm hook, a swayidle timeout. The point is ownership:
`~/go/bin` belongs to the Go toolchain and a release directory changes every
deploy, so neither is a name configuration should have to know. Pointing at the
published link means config is written once and the deploy owns the indirection.

`SWITCHBOARD_LINK_DIR` relocates the links, or disables them when set empty.

The links must be **symlinks, never copies**. A copy does not follow a deploy;
it keeps serving whatever revision it was copied from, indefinitely and
silently. `scripts/deploy` refuses to proceed when one of these names is a
regular file, and warns about any file under `~/.config` still naming a
deployed command in `~/go/bin`.

Do not use `go install` to deploy this project. It writes to `~/go/bin`, which
no unit reads, so the restart afterwards succeeds against the old binary.

## Deployed layout on goosebook

Recorded 2026-08-27 at `42485f2`. This host is Hyprland + Waybar;
`switchboard-polybar.service` is not installed.

| What | Path |
| --- | --- |
| Releases | `~/.local/share/switchboard/releases/<rev>/` |
| Live release | `~/.local/share/switchboard/current` |
| Published commands | `~/.local/bin/switchboard{,-ctl,-waybar}` |
| Host overlay (in repo) | `hosts/goosebook/systemd/` |

Both units take their values from the installed overlay:

```ini
# ~/.config/systemd/user/switchboard.service.d/20-machine.conf
[Service]
Environment=SWITCHBOARD_BIN=%h/.local/share/switchboard/current/switchboard
Environment="SWITCHBOARD_ARGS=-remote nlessfun"
```

```ini
# ~/.config/systemd/user/switchboard-waybar.service.d/20-machine.conf
[Service]
Environment=SWITCHBOARD_CTL=%h/.local/share/switchboard/current/switchboard-ctl
```

The federation peer belongs in that overlay and not in
`systemd/switchboard.service`: that file is shared across machines, its shipped
`SWITCHBOARD_ARGS` is pinned empty by `systemd/service_test.go`, and
`install -Dm644` reverts an in-place edit without a word. `hosts/hosts_test.go`
pins that the overlay keeps carrying a peer, and that no host ever points a unit
at `~/go/bin` or at a release directory — pinning a release would defeat the
flip.

### Drop-in ordering is a hazard

systemd merges drop-ins in **lexical filename order**, so a leftover file wins
on its name alone. `local-binary.conf` sorts after `20-machine.conf` and would
be applied last, holding both units on the previous binary path while the deploy
reported success. `scripts/deploy` detects a later drop-in setting
`SWITCHBOARD_BIN` or `SWITCHBOARD_CTL` and refuses with the removal command.

Keep one host's values in one drop-in for the same reason: splitting the binary
path and the argument list across two files makes the effective configuration
depend on their filenames.


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
