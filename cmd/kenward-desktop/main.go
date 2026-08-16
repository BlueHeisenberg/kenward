// Command kenward-desktop is the status-bar wrapper around the kenward daemon.
//
// It is a separate binary from `kenward` on purpose. The daemon must keep building
// with CGO_ENABLED=0, because the distroless container image has no libc to link
// against; a tray library needs cgo on macOS. Two artifacts out of one module keeps
// both true, and keeps the daemon the thing that actually runs a household —
// headless is first-class, and this binary is an addition nobody is required to
// install.
//
// What it does is deliberately small:
//
//   - supervises one `kenward run` child: starts it, restarts it when it dies,
//     stops it cleanly on quit;
//   - shows the state in the icon, so that glancing at the menu bar answers "is it
//     up";
//   - opens the dashboard in the user's real browser;
//   - reports `kenward doctor`'s findings verbatim under Status.
//
// It embeds no browser. A webview would buy a window frame and cost three platform
// integrations, a bundled runtime on Windows and a class of blank-white-window bugs;
// the default browser is already installed, already has the user's bookmarks and
// already works.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"fyne.io/systray"
)

// statusSlots is how many lines the Status submenu can hold.
//
// systray builds its menu once and mutates it, so the items are allocated up front
// and hidden when unused rather than added and removed per refresh. Twenty-four is
// comfortably more than a household-sized `kenward doctor` produces; anything past it
// is truncated with a line saying so, because a silently cut report is worse than a
// short one.
const statusSlots = 24

func main() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("kenward-desktop: ")

	// One flag, and only because the login entries need one. A Windows Run key
	// registry value cannot carry an environment variable, so the config path has
	// to travel on the command line if it is to travel at all, and the same form is
	// then used in the LaunchAgent and the XDG autostart entry so all three read the
	// same. Everything else this binary needs it works out for itself.
	configPath := flag.String("config", "", "path to kenward.yaml (default: $KENWARD_CONFIG, then ./kenward.yaml, then the per-OS config location)")
	flag.Parse()

	exe, err := kenwardBinary()
	if err != nil {
		// Nothing to supervise and nothing to report on. A tray icon that can only
		// say "I cannot find kenward" is worse than a message on the terminal that
		// says where it looked.
		log.Fatalf("%v", err)
	}
	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = resolveConfigPath()
	}

	app := &app{
		daemon:  newDaemon(exe, cfgPath),
		exe:     exe,
		cfg:     cfgPath,
		repaint: make(chan struct{}, 1),
		reports: make(chan statusReport),
	}
	systray.Run(app.onReady, app.onExit)
}

type app struct {
	daemon *daemon
	exe    string
	cfg    string

	mDashboard *systray.MenuItem
	mStatus    *systray.MenuItem
	mLogin     *systray.MenuItem
	slots      []*systray.MenuItem

	// Everything below is touched only by the loop in onReady.
	//
	// The state arrives from two other goroutines — the supervisor when the daemon's
	// state changes, the poller when doctor answers — and both hand it over through
	// the channels rather than writing here, so there is nothing to lock and no
	// menu item mutated from two places at once. It also means every repaint happens
	// on one goroutine, which is what a tray library wants and what none of them
	// promise to enforce.
	dashURL string
	report  statusReport
	repaint chan struct{}
	reports chan statusReport
}

func (a *app) onReady() {
	systray.SetIcon(iconFor(stateStopped))
	systray.SetTooltip("kenward — starting")

	a.mDashboard = systray.AddMenuItem("Open dashboard", "")
	a.mStatus = systray.AddMenuItem("Status", "")
	for range statusSlots {
		it := a.mStatus.AddSubMenuItem("", "")
		it.Disable()
		it.Hide()
		a.slots = append(a.slots, it)
	}
	a.mStatus.AddSeparator()
	// Start-at-login lives under Status rather than at the top level so the menu
	// stays the three items it is meant to be. It is a checkbox and it starts
	// unchecked on a fresh install: an application that adds itself to login without
	// being asked is one people uninstall.
	a.mLogin = a.mStatus.AddSubMenuItemCheckbox("Start at login", "", loginEnabled())
	mQuit := systray.AddMenuItem("Quit", "Drain the household and stop kenward")

	a.refreshDashboard()
	// Coalescing: a repaint is a request to redraw from whatever the state is by the
	// time it is served, so one pending request is as good as ten and a burst of
	// restarts cannot outrun the drawing.
	a.daemon.onChange = func(daemonState) {
		select {
		case a.repaint <- struct{}{}:
		default:
		}
	}
	a.daemon.start()
	a.render()

	go a.pollStatus()

	for {
		select {
		case <-a.repaint:
			a.render()
		case rep := <-a.reports:
			a.report = rep
			a.refreshDashboard()
			a.render()
		case <-a.mDashboard.ClickedCh:
			a.openDashboard()
		case <-a.mLogin.ClickedCh:
			a.toggleLogin()
		case <-mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// onExit drains the household before the process goes away.
//
// It blocks for as long as the daemon's own drain takes, and the tooltip says so.
// Killing the child instead would be instant and would cut whatever turn was in
// flight, leaving a member's message half-answered and their session key unlocked in
// a process that no longer exists.
func (a *app) onExit() {
	systray.SetTooltip("kenward — stopping, finishing in-flight turns")
	a.daemon.stop()
}

// pollStatus re-runs `kenward doctor` on a slow timer.
//
// Slow because doctor is not free: it authorises every bot token with Telegram and
// probes every endpoint. Whether the daemon is alive is known instantly and for
// nothing — this process owns it — so the timer only governs the richer facts, and
// the daemon's own state changes force a refresh out of band.
func (a *app) pollStatus() {
	a.reports <- runDoctor(a.exe, a.cfg)
	for range time.Tick(statusInterval) {
		a.reports <- runDoctor(a.exe, a.cfg)
	}
}

const statusInterval = 5 * time.Minute

func (a *app) refreshDashboard() {
	a.dashURL = dashboardURL(a.cfg)
	if a.mDashboard == nil {
		return
	}
	if a.dashURL == "" {
		// Greyed out and told why, rather than opening a URL nothing is listening
		// on and handing the user a browser error page to interpret.
		a.mDashboard.SetTitle("Open dashboard (not enabled)")
		a.mDashboard.SetTooltip("No dashboard is configured in " + a.cfg)
		a.mDashboard.Disable()
		return
	}
	a.mDashboard.SetTitle("Open dashboard")
	a.mDashboard.SetTooltip(a.dashURL)
	a.mDashboard.Enable()
}

func (a *app) openDashboard() {
	if a.dashURL == "" {
		return
	}
	if err := openBrowser(a.dashURL); err != nil {
		log.Printf("opening %s: %v", a.dashURL, err)
	}
}

func (a *app) toggleLogin() {
	want := !a.mLogin.Checked()
	if err := setLoginEnabled(want, a.cfg); err != nil {
		log.Printf("start at login: %v", err)
		return
	}
	if want {
		a.mLogin.Check()
		return
	}
	a.mLogin.Uncheck()
}

// render paints the current state into the icon, the tooltip and the Status submenu.
//
// A live child is necessary but not sufficient for the green icon. `kenward doctor`
// exits non-zero for things that leave the daemon running and useless — a bot token
// Telegram has stopped authorising, a lore space the store no longer holds — and an
// icon that stayed green through those would be decoration. So the icon is the worse
// of the two verdicts, and the Status submenu underneath says which one it was.
func (a *app) render() {
	st, detail := a.daemon.snapshot()
	if st == stateRunning && a.report.settled() && !a.report.healthy() {
		st = stateFailed
	}
	systray.SetIcon(iconFor(st))
	// No tooltip reaches a Linux tray — StatusNotifierItem has one but fyne does not
	// wire it — which is why the first line of the Status submenu repeats it.
	systray.SetTooltip("kenward — " + detail)

	lines := []string{detail}
	lines = append(lines, a.report.lines()...)

	for i, it := range a.slots {
		if i >= len(lines) {
			it.Hide()
			continue
		}
		if i == len(a.slots)-1 && len(lines) > len(a.slots) {
			it.SetTitle(fmt.Sprintf("… %d more; run `kenward doctor`", len(lines)-len(a.slots)+1))
			it.Show()
			continue
		}
		it.SetTitle(lines[i])
		it.Show()
	}
}

// kenwardBinary finds the daemon this wrapper supervises.
//
// Beside this executable first, because that is where every one of the platform
// bundles puts the pair: the .app's MacOS directory, the Windows install directory,
// /usr/bin from the .deb. PATH second, for a developer who has built one of them into
// ./bin and has the other on PATH. KENWARD_BINARY overrides both, which is how the
// tests point it at a stub.
func kenwardBinary() (string, error) {
	if v := os.Getenv("KENWARD_BINARY"); v != "" {
		return v, nil
	}
	name := "kenward"
	if isWindows {
		name += ".exe"
	}
	self, err := os.Executable()
	if err == nil {
		beside := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(beside); err == nil {
			return beside, nil
		}
	}
	if p, err := lookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no %s found beside this program or on PATH; "+
		"kenward-desktop supervises the kenward daemon and cannot do anything without it", name)
}
