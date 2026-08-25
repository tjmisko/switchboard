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
