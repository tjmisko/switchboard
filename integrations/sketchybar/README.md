# SketchyBar integration (macOS)

The macOS renderer for Seam 4. Like the waybar renderer it consumes only the
public contract — the snapshot printed by `switchboard-ctl --json list` — so it
knows nothing about the process, terminal or WM layers beneath it.

## Install

```sh
brew tap FelixKratz/formulae
brew trust --formula felixkratz/formulae/sketchybar   # third-party tap
brew install sketchybar

mkdir -p ~/.config/sketchybar/plugins
cp integrations/sketchybar/sketchybarrc ~/.config/sketchybar/
cp integrations/sketchybar/plugins/switchboard.sh ~/.config/sketchybar/plugins/
chmod +x ~/.config/sketchybar/sketchybarrc ~/.config/sketchybar/plugins/switchboard.sh

brew services start felixkratz/formulae/sketchybar
```

Point the plugin at your binary if it is not on the default path:

```sh
export SWITCHBOARD_CTL=/path/to/switchboard-ctl
```

`jq` is required.

## What it renders

One chip per session, coloured by status — red `permission`, orange `idle`,
green `working`, blue `delegating`, grey unknown. The focused session gets a
background. Clicking a chip runs `switchboard-ctl focus pid:<n>`.

The summary item on the left shows the session count and appends `· observe
only` when `capabilities.navigate` is false. That is the normal state on stock
macOS: chips still report status, but clicking cannot raise a window. See
`docs/macos-port/00-synthesis.md` §7 for what unlocks window-level Navigate.

## Status

Chips depend on session discovery, which needs a working `osproc` darwin
backend. Until that lands the bar renders correctly and reports
`0 session(s) · observe only`.

## Running the daemon at login

`launchd/io.github.tjmisko.switchboard.plist` is the macOS analogue of
`systemd/switchboard.service`. launchd does no variable expansion in
`ProgramArguments`, so substitute the home directory when installing:

```sh
sed "s|__HOME__|$HOME|g" launchd/io.github.tjmisko.switchboard.plist \
  > ~/Library/LaunchAgents/io.github.tjmisko.switchboard.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.github.tjmisko.switchboard.plist
```

`scripts/deploy` does not run here: it requires `systemctl`, reads `/proc` to
verify the running revision, and uses `mv -T`, which is a GNU extension absent
from BSD `mv`. Install by hand until a macOS deploy path exists.
