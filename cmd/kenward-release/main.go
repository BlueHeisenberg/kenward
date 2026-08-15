package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// usageError marks a failure in how the command was invoked or configured,
// as opposed to something going wrong while it ran. docs/CLI.md maps the
// former to exit code 2 and the latter to 1.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usagef(format string, a ...any) error {
	return &usageError{err: fmt.Errorf(format, a...)}
}

const rootUsage = `kenward-release — build, sign and check kenward release manifests.

This binary is not shipped to households. It holds the capability to sign
releases, which is the root of trust for every installation.

Usage:
  kenward-release <command> [flags]

Commands:
  keygen     Create a new Ed25519 release signing keypair.
  manifest   Build an unsigned release manifest from a directory of builds.
  sign       Sign a manifest, or add a second signature to a signed one.
  verify     Check a signed manifest and print exactly what it says.
  version    Print this tool's build information.

Run "kenward-release <command> -h" for what a command does and the flags it
takes.

Exit codes: 0 success, 1 something failed, 2 you invoked it wrongly.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable body: everything it writes goes to stdout or
// stderr, and it returns the process exit code rather than calling os.Exit.
func run(args []string, stdout, stderr io.Writer) int {
	err := dispatch(args, stdout, stderr)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		// Help was asked for and printed. That is not a failure.
		return 0
	}
	fmt.Fprintf(stderr, "kenward-release: %v\n", err)
	var ue *usageError
	if errors.As(err, &ue) {
		return 2
	}
	return 1
}

func dispatch(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, rootUsage)
		return usagef("no command given")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "keygen":
		return cmdKeygen(rest, stdout, stderr)
	case "manifest":
		return cmdManifest(rest, stdout, stderr)
	case "sign":
		return cmdSign(rest, stdout, stderr)
	case "verify":
		return cmdVerify(rest, stdout, stderr)
	case "version":
		return cmdVersion(rest, stdout, stderr)
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, rootUsage)
		return nil
	default:
		fmt.Fprint(stderr, rootUsage)
		return usagef("unknown command %q", cmd)
	}
}

// parseFlags wires a command's flag set to the writers under test, answers an
// explicit -h on stdout, and turns any parse failure into a usage error so it
// exits 2 rather than flag's own os.Exit.
func parseFlags(fs *flag.FlagSet, usage string, args []string, stdout, stderr io.Writer) error {
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage) }
	if wantsHelp(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return flag.ErrHelp
	}
	fs.SetOutput(io.Discard) // we print the usage and the error ourselves
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(stderr, usage)
		return &usageError{err: err}
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return usagef("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "--help", "-help", "help":
			return true
		case "--":
			return false
		}
	}
	return false
}

// stringList collects a flag that may be repeated, and also accepts a single
// comma-separated value, so "--pub a.pub --pub b.pub" and "--pub a.pub,b.pub"
// mean the same thing.
type stringList []string

func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

func requireFlag(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return usagef("--%s is required", name)
	}
	return nil
}
