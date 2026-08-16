# The desktop wrapper

`kenward-desktop` is a status-bar application that supervises the kenward daemon and
gives somebody a way to reach it without a terminal. It is optional. Every household
that runs kenward headless — as a systemd unit, in a container, over SSH — is
unaffected by everything on this page, and nothing here is required to use kenward.

It is a **separate binary** from `kenward`, and that is the load-bearing decision. The
daemon must keep building with `CGO_ENABLED=0`, because the distroless image has no
libc to link against; a menu bar needs cgo on macOS. Two artifacts out of one module
keeps both true. `cmd/kenward-desktop` imports one constant from `internal/setup` and
nothing else of kenward's: it starts `kenward run` as a child process and asks
`kenward doctor` for the facts.

## What it does

The menu is three items and stays three items.

```
Open dashboard          opens http://127.0.0.1:<port> in your own browser
Status              ▸   what `kenward doctor` says, plus Start at login
Quit                    drains the household, then exits
```

**There is no embedded browser.** "Open dashboard" launches the default browser at the
address the configuration names. A WebView2 or WKWebView window would buy a frame
around a page and cost three platform integrations, a bundled runtime on Windows and a
class of blank-white-window bugs. The browser is already installed, already has the
household's bookmarks and already works.

When the configuration names no dashboard the item is disabled and says so, rather
than opening a URL nothing is listening on. The address is read from `kenward.yaml`
every time the status refreshes, so enabling the dashboard does not need the wrapper
restarted.

## Supervision

The wrapper owns exactly one `kenward run` child.

- **Start.** One child, with `--config` pointing at the same file the wrapper read.
- **Restart.** A child that exits is started again. If it survived thirty seconds it is
  restarted immediately, because whatever ended it is not the kind of fault that fails
  instantly on the next start — and because that is the path an auto-update takes:
  `kenward run` exits non-zero on purpose after swapping its own binary, expecting a
  service manager to bring it back, and here the wrapper is that service manager. If it
  died sooner the wait doubles from one second up to thirty.
- **It never gives up.** There is no failure state that stops retrying. A restart loop
  usually means an endpoint is down, a machine is asleep or a token was rotated, and
  all of those fix themselves when the cause does. Retrying forever with a red icon is
  better than a grey icon that needs somebody to know what to do about it.
- **Quit drains.** The child is asked to stop, not killed: SIGTERM on macOS and Linux,
  a `CTRL_BREAK` console control event on Windows. `kenward run` turns either into a
  drain — intake stops, in-flight turns finish, every session key is zeroed — and the
  wrapper waits up to three minutes for it, matching the daemon's own drain timeout.
  Only after that does it kill.

One thing it does not do: if the wrapper itself is killed outright, the daemon survives
as an orphan. A Windows job object or a Unix process group would fix that and neither
is worth the complexity for a program whose normal exit is the Quit item.

## Icon states

Three, and they differ in shape as well as colour, so they stay readable to a
colour-blind user and on a panel that recolours icons to match the theme.

| | Shape | Meaning |
| --- | --- | --- |
| Green | filled disc | the child is running **and** `kenward doctor` exits 0 |
| Grey | hollow ring | stopped: before the first start, and while quitting |
| Red | struck-through disc | the child will not stay up, or doctor exits non-zero |

A live daemon is necessary but not sufficient for green. `kenward doctor` exits
non-zero for faults that leave the process running and useless — a bot token Telegram
has stopped authorising, a lore space the store no longer holds — and an icon that
stayed green through those would be decoration.

## Status

The Status submenu is `kenward doctor --json`, rendered in doctor's own words with
doctor's own symbols. Nothing is paraphrased, because a tray that paraphrases is a tray
that eventually paraphrases wrongly and disagrees with the command you run when it
worries you. Its exit codes are the contract: **0** success, **1** a runtime failure,
**2** a configuration or usage error.

It re-runs every five minutes, not every few seconds: doctor authorises every bot token
with Telegram and probes every endpoint. Whether the child process is alive is known
instantly and for free, so the slow timer only governs the richer facts, and any change
in the daemon's own state refreshes the menu out of band.

## Start at login

A checkbox inside the Status submenu, off on a fresh install. An application that adds
itself to login without being asked is one people uninstall.

| | Mechanism |
| --- | --- |
| Windows | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` — the same entry Task Manager's Startup tab lists |
| macOS | `~/Library/LaunchAgents/io.kenward.desktop.plist` |
| Linux | `~/.config/autostart/kenward-desktop.desktop` |

All three are per-user and need no administrator. The state is read back from the
mechanism rather than remembered, so turning it off in Task Manager or GNOME Tweaks
leaves the checkbox correct.

Turning it on means "start next time I log in". Nothing is bootstrapped into the
running session, and no mechanism is configured to restart the wrapper if it dies —
launchd or the session restarting the wrapper while the wrapper restarts the daemon is
two supervisors racing over one household.

## Linux: GNOME needs an extension

GNOME Shell removed the XEmbed notification area in 3.26 and never adopted
StatusNotifierItem. As of GNOME 50 there is still no tray, and no way to get one on
stock GNOME without a shell extension. This is Shell policy, not a Wayland limitation.

- **KDE Plasma** consumes StatusNotifierItem natively. Nothing to install.
- **Ubuntu** ships the AppIndicator extension enabled, which is why it works there out
  of the box and not on Fedora or Arch.
- **Stock GNOME** needs either
  [Status Tray](https://extensions.gnome.org/extension/9164/status-tray/) or
  [AppIndicator and KStatusNotifierItem Support](https://extensions.gnome.org/extension/615/appindicator-support/).
- **Xfce, Cinnamon, MATE** work through their own indicator plugins.

The wrapper checks for `org.kde.StatusNotifierWatcher` on the session bus at startup
and logs a line naming the extensions when it is absent, because the failure without
that check is the worst kind: the process starts, reports no error, supervises the
household perfectly, and the user sees nothing and concludes it is broken. The daemon
is supervised either way.

There is also no tooltip on Linux — StatusNotifierItem has one and the library does not
wire it — which is why the first line of the Status submenu repeats what the tooltip
says elsewhere.

## Building

The daemon's pure-Go build is the thing to protect. Check both:

```sh
GOWORK=off CGO_ENABLED=0 go build ./cmd/kenward      # must always work
GOWORK=off go build ./cmd/kenward-desktop
GOWORK=off go test -count=1 ./...
```

| Target | cgo | Notes |
| --- | --- | --- |
| `windows/amd64` | not needed | link with `-ldflags -H=windowsgui`, or a console window appears |
| `linux/amd64`, `linux/arm64` | not needed | pure-Go D-Bus; no GTK, no libayatana, no X11 headers |
| `darwin/amd64`, `darwin/arm64` | **required** | `CGO_ENABLED=1` on a macOS builder; not cross-compilable |

## Packaging

Everything below is **unsigned**. Apple notarisation and Windows code signing are out
of scope, and the consequence is a scary dialogue on first launch that users must be
told about rather than left to discover.

### macOS — `.app` inside a `.dmg`

`packaging/macos/bundle.sh VERSION DESKTOP_BINARY KENWARD_BINARY [OUTDIR]`, run on
macOS. It assembles `kenward.app` with both binaries inside `Contents/MacOS` — the
wrapper looks for the daemon beside its own executable, so dragging the app to
Applications gives a working pair with nothing on `PATH` — builds the `.icns` from
`packaging/kenward.png` with `sips` and `iconutil`, and wraps the result in a `.dmg`
alongside an Applications symlink and `FIRST-LAUNCH.txt`.

`Info.plist` sets `LSUIElement`, so it is a menu bar agent with no Dock tile.

**What the user sees.** Double-clicking an unsigned app fails with a message that reads
as though the download is damaged. The first launch has to be Finder →
right-click → **Open** → **Open**. On macOS 15 and later even that may be refused, and
the route is System Settings → Privacy & Security → **Open Anyway**. macOS remembers;
every launch after the first is ordinary. `FIRST-LAUNCH.txt` ships inside the `.dmg`
saying exactly this.

### Windows — installer

`packaging/windows/kenward.iss`, compiled with Inno Setup:

```
iscc /DVersion=0.1.0 /DSourceDir=..\..\dist\windows_amd64 packaging\windows\kenward.iss
```

`SourceDir` must hold `kenward-desktop.exe` and `kenward.exe`; both are installed into
one directory, for the same beside-the-executable reason as macOS.
`PrivilegesRequired=lowest`, so it is a per-user install with no UAC prompt. Start at
login is an unchecked task writing the same `HKCU` Run value the tray's checkbox
writes, so the two agree.

**What the user sees.** SmartScreen blocks an unsigned installer with "Windows
protected your PC" and hides the run button behind **More info** → **Run anyway**. The
installer's finish page says why. Separately, Windows 11 hides new tray icons in the
overflow: the icon is behind the `^` chevron beside the clock until it is dragged out.

### Linux — `.deb` and `.rpm`

`packaging/linux/nfpm.yaml`, with the binaries staged into `dist/nfpm/`:

```sh
mkdir -p dist/nfpm && cp dist/linux_amd64/kenward dist/linux_amd64/kenward-desktop dist/nfpm/
VERSION=0.1.0 ARCH=amd64 nfpm pkg -f packaging/linux/nfpm.yaml -p deb -t dist/
VERSION=0.1.0 ARCH=amd64 nfpm pkg -f packaging/linux/nfpm.yaml -p rpm -t dist/
```

nfpm expands `${VERSION}` and `${ARCH}` but not paths inside `contents`, which is why
the binaries are staged rather than pointed at.

The packages install `/usr/bin/kenward-desktop`, `/usr/bin/kenward`, the `.desktop`
entry and the hicolor icon. They declare **no dependencies** — the tray is pure-Go
D-Bus — and recommend the GNOME AppIndicator extension, which no distribution packages
under one name, which is why the program says it itself.

There is deliberately no systemd unit in this package. `deploy/kenward.service` is how
a household runs the daemon headless; this package is the desktop wrapper, which
supervises its own child in the user's session. Installing both would put two
supervisors on one household.

### The icons

`packaging/kenward.png` and `packaging/kenward.ico` are generated by
`go run packaging/mkicon.go`. They are committed so a build needs no extra step, and
the generator is committed so they can be changed — a binary nobody can regenerate is a
binary nobody can review. The mark is a placeholder ring, deliberately in neither of
the tray's status colours.

## What the release pipeline has to do

Owned elsewhere (`.goreleaser.yml`, `.github/workflows/`); listed here so it is not
guessed at.

1. **Build `cmd/kenward-desktop` per platform.** `CGO_ENABLED=0` for
   `windows/amd64`, `linux/amd64` and `linux/arm64`; `CGO_ENABLED=1` on a
   **macOS runner** for `darwin/amd64` and `darwin/arm64`. Darwin cannot be
   cross-compiled from Linux, so the release needs a `macos-latest` job. The existing
   `cmd/kenward` matrix is unchanged and must stay `CGO_ENABLED=0`.
2. **Link Windows with `-ldflags "-H=windowsgui"`** in addition to the version
   `-X` flags, or every launch opens a console window.
3. **Ship the daemon beside the wrapper** in all three bundles. Both binaries, one
   directory.
4. **Run the three packagers**: `packaging/macos/bundle.sh` on the macOS runner,
   `iscc` on the Windows runner (Inno Setup is preinstalled on GitHub's
   `windows-latest` image), and nfpm — natively or through goreleaser's own `nfpms:`
   block, translating `packaging/linux/nfpm.yaml`.
5. **Publish**: `kenward_<version>_macos.dmg`,
   `kenward_<version>_windows_amd64_setup.exe`, `kenward-desktop_<version>_<arch>.deb`
   and `kenward-desktop-<version>-1.<arch>.rpm`, plus the bare binaries for anyone who
   wants them.
6. **Do not sign or notarise.** Out of scope by decision. The install page should carry
   the same right-click→Open and SmartScreen notes as above, so a user meets the
   warning already knowing what it is.
7. **The container image is untouched.** `cmd/kenward-desktop` must never enter the
   Dockerfile: it would pull a tray library into a distroless image with no display.
