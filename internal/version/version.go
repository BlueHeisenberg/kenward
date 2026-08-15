// Package version exposes build metadata for the kenward binary.
//
// The variables below are intended to be overridden at link time via
// -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/BlueHeisenberg/kenward/internal/version.Version=v0.1.0 \
//	  -X github.com/BlueHeisenberg/kenward/internal/version.Commit=abc1234 \
//	  -X github.com/BlueHeisenberg/kenward/internal/version.Date=2026-08-15T00:00:00Z"
//
// When built without ldflags (e.g. `go build` or `go run` during local
// development), the zero-value defaults below are used instead.
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semantic version of this build, typically derived
	// from `git describe --tags --always --dirty`. Defaults to "dev"
	// when not injected at link time.
	Version = "dev"

	// Commit is the short git commit hash this build was produced from.
	// Defaults to "none" when not injected at link time.
	Commit = "none"

	// Date is the UTC build timestamp. Defaults to "unknown" when not
	// injected at link time.
	Date = "unknown"
)

// Short returns the version string alone, e.g. "v0.1.0".
func Short() string {
	return Version
}

// Full returns a human-readable, single-line description of the build
// suitable for --version output and startup logs, including the version,
// commit, build date, Go runtime version, and target platform, e.g.:
//
//	kenward v0.1.0 (abc1234, 2026-08-15, go1.25.0, linux/amd64)
func Full() string {
	return fmt.Sprintf(
		"kenward %s (%s, %s, %s, %s/%s)",
		Version,
		Commit,
		Date,
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
	)
}
