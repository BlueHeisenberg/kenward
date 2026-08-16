package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/godbus/dbus/v5"
)

func openBrowser(url string) error { return exec.Command("xdg-open", url).Start() }

// --- will anything actually show this icon? ---------------------------------------

// warnIfNoTrayHost says so, once, when this desktop has nowhere to put a tray icon.
//
// GNOME Shell has had no notification area since 3.26 and still has none in GNOME 50:
// it dropped XEmbed and never adopted StatusNotifierItem, so on stock GNOME a tray
// application is simply invisible. Ubuntu ships the AppIndicator extension enabled,
// which is why it appears to work there and not on Fedora or Arch. KDE Plasma consumes
// StatusNotifierItem natively and needs nothing.
//
// The failure mode without this check is the worst kind: the process starts, reports
// no error, supervises the household perfectly, and the user sees nothing at all and
// concludes the program is broken. One line naming the extension turns that into a
// five-minute fix.
func warnIfNoTrayHost() {
	conn, err := dbus.SessionBus()
	if err != nil {
		log.Printf("no session bus: a tray icon needs one, and nothing will be visible (%v)", err)
		return
	}
	var owned []string
	if err := conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&owned); err != nil {
		return
	}
	for _, name := range owned {
		if name == "org.kde.StatusNotifierWatcher" {
			return
		}
	}
	log.Print("no org.kde.StatusNotifierWatcher on the session bus: this desktop has no " +
		"tray for the icon to appear in. GNOME has shipped without one since 3.26 — install " +
		"the \"Status Tray\" or \"AppIndicator and KStatusNotifierItem Support\" shell " +
		"extension. kenward itself is unaffected: the daemon is supervised either way.")
}

func init() { warnIfNoTrayHost() }

// --- start at login ---------------------------------------------------------------
//
// The XDG autostart directory, which GNOME, KDE, Xfce and everything else that
// implements the specification read at login, and which GNOME Tweaks and KDE's
// Autostart panel both list — so a user who turns it off there is turning off the same
// thing this menu item shows.

const autostartFile = "kenward-desktop.desktop"

func autostartPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", autostartFile), nil
}

func loginEnabled() bool {
	path, err := autostartPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func setLoginEnabled(on bool, configPath string) error {
	path, err := autostartPath()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(autostartEntry(self, configPath)), 0o644)
}

// autostartEntry is the same shape as the .desktop file the packages install, with
// one difference that matters: X-GNOME-Autostart-Delay. The session's tray host is
// itself a shell extension and is not always up when autostart fires; an icon
// registered before the watcher exists is an icon nobody draws.
func autostartEntry(exe, configPath string) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=kenward
Comment=Supervise the kenward household assistant
Exec=%s --config %s
Icon=kenward
Terminal=false
Categories=Utility;
X-GNOME-Autostart-enabled=true
X-GNOME-Autostart-Delay=5
`, exe, configPath)
}
