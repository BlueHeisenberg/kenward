//go:build unix && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package setup

import "golang.org/x/sys/unix"

// The ioctl numbers for reading and writing terminal attributes on Linux and the
// other System V derived platforms, which is where kenward is actually deployed.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
