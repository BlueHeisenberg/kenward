package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const isWindows = true

var lookPath = exec.LookPath

// --- stopping the daemon cleanly -------------------------------------------------
//
// Windows has no SIGTERM. The one way to ask a console program to shut down is a
// console control event, and `kenward run` already handles it: Go's runtime delivers
// CTRL_BREAK_EVENT as os.Interrupt, which is what its signal.NotifyContext waits for,
// and which it turns into a drain.
//
// Three things have to line up for that to work, and all three are here:
//
//  1. this process must own a console, which a program linked with -H=windowsgui does
//     not — hence attachConsole below;
//  2. the child must be in its own process group, so the event reaches it and not us;
//  3. the event must be CTRL_BREAK rather than CTRL_C, because CTRL_C cannot be sent
//     to a specific group at all.

func newProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

func interrupt(p *os.Process) error {
	// The process id doubles as the group id for a group created with
	// CREATE_NEW_PROCESS_GROUP.
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.Pid))
}

// notifyTerm is used only by the test helper process. A console control event arrives
// as os.Interrupt, for which the helper has already registered, so there is nothing
// further to ask for here.
func notifyTerm(chan<- os.Signal) {}

// x/sys/windows exposes neither of the console-window calls, so the three used below
// are bound here. All are in kernel32/user32 and have been since Windows 2000.
var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	user32            = windows.NewLazySystemDLL("user32.dll")
	procAllocConsole  = kernel32.NewProc("AllocConsole")
	procConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procShowWindow    = user32.NewProc("ShowWindow")
)

const swHide = 0

// attachConsole gives this process the hidden console it needs in order to be allowed
// to send that event, and is called once at startup.
//
// Run from a terminal, AllocConsole fails because there is already one, and that is
// the good case: the console is the user's, it stays visible, and the daemon's log
// lines land in it. Run from a shortcut there is none, so one is allocated and hidden
// immediately. Hiding only what we allocated is the important part — hiding a console
// we did not create would make the user's own terminal window disappear.
func attachConsole() {
	if ok, _, _ := procAllocConsole.Call(); ok == 0 {
		// Already had one: we were launched from a terminal. Leave it visible —
		// it is the user's window, and the daemon's log lines belong in it.
		return
	}
	hwnd, _, _ := procConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

func init() { attachConsole() }

// --- opening the browser ---------------------------------------------------------

// rundll32 rather than `cmd /c start`, which treats a quoted first argument as a
// window title and mangles URLs containing an ampersand.
func openBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// --- start at login ---------------------------------------------------------------
//
// The Run key under HKCU, which is the per-user, no-elevation, user-visible place for
// this: it is what Task Manager's Startup tab lists and lets people turn off, so a
// household that disables it there and finds the menu still ticked would be looking
// at a lie. Reading the key back rather than remembering our own answer is what keeps
// those two in step.

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "kenward-desktop"

func loginEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	return err == nil && v != ""
}

func setLoginEnabled(on bool, configPath string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err := k.DeleteValue(runValue)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Quoted: the default install path has a space in it, and an unquoted path
	// under Program Files is the classic way a Run entry launches the wrong thing.
	return k.SetStringValue(runValue, fmt.Sprintf("%q --config %q", self, filepath.Clean(configPath)))
}

// --- icons -------------------------------------------------------------------------

// packIcon wraps a PNG in an ICO container, because Windows loads the tray icon
// through LoadImage from a file and LoadImage wants an .ico.
//
// A PNG inside an ICO is the modern form and has been understood since Vista, so the
// container is a fixed twenty-two byte header and nothing else: one directory entry
// pointing at the image bytes as they already are.
func packIcon(png []byte) []byte {
	if png == nil {
		return nil
	}
	const headerLen = 6 + 16
	out := make([]byte, headerLen, headerLen+len(png))
	// ICONDIR: reserved, type 1 (icon), one image.
	binary.LittleEndian.PutUint16(out[0:], 0)
	binary.LittleEndian.PutUint16(out[2:], 1)
	binary.LittleEndian.PutUint16(out[4:], 1)
	// ICONDIRENTRY. A width or height of 256 is written as zero; iconSize is 32,
	// so the plain value is correct.
	out[6] = byte(iconSize)
	out[7] = byte(iconSize)
	out[8] = 0                                  // not palettised
	out[9] = 0                                  // reserved
	binary.LittleEndian.PutUint16(out[10:], 1)  // colour planes
	binary.LittleEndian.PutUint16(out[12:], 32) // bits per pixel
	binary.LittleEndian.PutUint32(out[14:], uint32(len(png)))
	binary.LittleEndian.PutUint32(out[18:], headerLen)
	return append(out, png...)
}
