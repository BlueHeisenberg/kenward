package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/version"
)

const versionUsage = `kenward-release version

Prints this tool's version, commit, build date, Go version and platform, on one
line. It says nothing about the release you are cutting — for that, run
"kenward-release verify".
`

func cmdVersion(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := parseFlags(fs, versionUsage, args, stdout, stderr); err != nil {
		return err
	}
	// internal/version formats its line for the kenward binary. This is the
	// same build of the same tree, so only the program name differs.
	fmt.Fprintln(stdout, strings.Replace(version.Full(), "kenward ", "kenward-release ", 1))
	return nil
}
