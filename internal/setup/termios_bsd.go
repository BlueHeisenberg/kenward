//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package setup

import "golang.org/x/sys/unix"

// The ioctl numbers for reading and writing terminal attributes on the BSDs, macOS
// among them. They are split out per platform family because the constants differ
// and there is no portable name for them.
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
