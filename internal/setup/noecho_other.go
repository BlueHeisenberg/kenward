//go:build !unix && !windows

package setup

import "os"

// noEchoFor reports that this platform has no console this package knows how to
// silence, so AskSecret reads plainly.
//
// It returns nil rather than a function that pretends: a build for a platform
// nobody has tried must not claim a value was hidden when it was shown.
func noEchoFor(f *os.File) func(func() error) error { return nil }
