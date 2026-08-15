package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/BlueHeisenberg/kenward/internal/version"
)

// versionReport is `kenward version --json`.
type versionReport struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

func cmdVersion(e *env, args []string) int {
	fs := newFlagSet(e, "version", "kenward version [--json]")
	asJSON := fs.Bool("json", false, "emit the build as JSON")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		e.errorf("version takes no positional arguments; got %q", fs.Arg(0))
		return exitUsage
	}

	if *asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(versionReport{
			Version:  version.Version,
			Commit:   version.Commit,
			Date:     version.Date,
			Go:       runtime.Version(),
			Platform: runtime.GOOS + "/" + runtime.GOARCH,
		}); err != nil {
			e.errorf("%v", err)
			return exitFailure
		}
		return exitOK
	}
	fmt.Fprintln(e.stdout, version.Full())
	return exitOK
}
