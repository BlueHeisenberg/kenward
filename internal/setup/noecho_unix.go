//go:build unix

package setup

import (
	"os"

	"golang.org/x/sys/unix"
)

// noEchoFor returns a function that runs read with terminal echo disabled, or nil
// when f is not a terminal.
//
// ECHO is cleared and restored around the read, and restored on failure too: a
// program that exits leaving a terminal with echo off has broken the shell it was
// launched from, which is a worse bug than the one that made it exit.
//
// ECHONL is set so the newline the operator types is still shown. Without it the
// cursor stays at the end of the prompt and the terminal looks hung at exactly the
// moment somebody has just pasted a secret and is wondering whether it took.
func noEchoFor(f *os.File) func(func() error) error {
	fd := int(f.Fd())
	before, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil
	}
	return func(read func() error) error {
		during := *before
		during.Lflag &^= unix.ECHO
		during.Lflag |= unix.ECHONL
		if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &during); err != nil {
			return err
		}
		defer unix.IoctlSetTermios(fd, ioctlWriteTermios, before)
		return read()
	}
}
