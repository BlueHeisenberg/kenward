//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const isWindows = false

var lookPath = exec.LookPath

// newProcessGroup is deliberately nothing here.
//
// On Windows the child needs its own process group so a console control event can be
// aimed at it. On Unix there is no such constraint, and putting the child in its own
// group would have a cost: a Ctrl-C in the terminal a developer launched this from
// would reach the wrapper and not the daemon, so the daemon would outlive it. Sharing
// the group means both get the signal and both shut down, which is what anyone
// pressing Ctrl-C meant.
func newProcessGroup(*exec.Cmd) {}

// interrupt asks the daemon to drain. `kenward run` turns SIGTERM into a drain: intake
// stops, in-flight turns finish, every session key is zeroed.
func interrupt(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// packIcon is the identity outside Windows: both the macOS menu bar and the
// StatusNotifierItem specification take the PNG as it stands.
func packIcon(png []byte) []byte { return png }

// notifyTerm is used only by the test helper process, which has to wait for whatever
// interrupt does on this platform. On Windows that is a console control event, which
// arrives as os.Interrupt and needs nothing extra; here it is SIGTERM.
func notifyTerm(c chan<- os.Signal) { signal.Notify(c, syscall.SIGTERM) }
