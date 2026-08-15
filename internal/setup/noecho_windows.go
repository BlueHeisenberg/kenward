package setup

import (
	"os"

	"golang.org/x/sys/windows"
)

// noEchoFor returns a function that runs read with console echo disabled, or nil
// when f is not a console.
//
// Windows console input is a mode word on the handle rather than a terminal
// discipline, so ENABLE_ECHO_INPUT is cleared for the duration of the read and
// restored afterwards — including when the read fails, because leaving a console
// with echo off is how somebody ends up typing blind into their own shell.
//
// ENABLE_LINE_INPUT is deliberately left alone: the console keeps doing line
// editing, so backspace still works while the token is being pasted.
func noEchoFor(f *os.File) func(func() error) error {
	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return nil
	}
	return func(read func() error) error {
		if err := windows.SetConsoleMode(handle, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
			return err
		}
		defer windows.SetConsoleMode(handle, mode)
		return read()
	}
}
