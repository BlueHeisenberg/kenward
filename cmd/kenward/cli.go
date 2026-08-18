package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// Exit codes, from docs/CLI.md's conventions. They are the whole of this binary's
// contract with a shell script and with the container's HEALTHCHECK, so they are
// named rather than written as literals at the call sites.
const (
	// exitOK means the command did what it was asked to.
	exitOK = 0
	// exitFailure means something the operator did not control went wrong: lore did
	// not answer, Telegram refused the token, a supervisor died.
	exitFailure = 1
	// exitUsage means the fault is in the configuration or on the command line.
	// Nothing was attempted.
	exitUsage = 2

	// exitRestartRequested is what `run` exits with after an update has been
	// installed, or a failed one rolled back, and the process has to come back on
	// a different binary.
	//
	// It is deliberately the runtime-failure code rather than a fourth one.
	// deploy/kenward.service sets Restart=on-failure, so exiting 0 here would
	// install a new version and then leave the household with nothing running
	// until somebody noticed. Compose's restart: unless-stopped restarts on any
	// exit, so it is unaffected either way. The log line immediately before says
	// what happened, which is what an operator reading journalctl actually needs;
	// a distinct code that systemd ignored would not be.
	exitRestartRequested = exitFailure
)

// env is everything a command needs from the world outside it.
//
// It exists so that every command is a pure function of its arguments and this
// struct. A test supplies buffers, a fake environment and a frozen clock; main
// supplies the process's own. Nothing below reaches for os.Stdout, os.LookupEnv or
// time.Now directly, because a command that does cannot be asserted on.
type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// lookupEnv reads the process environment. Injected so that a test can prove
	// the missing-variable path without mutating global state shared with every
	// other test in the binary.
	lookupEnv config.LookupEnvFunc
	// secretOpts seams the secret resolver: the filesystem secret files are read
	// from, and the credentials directory. LookupEnv is filled from lookupEnv when
	// it is left nil, so a caller that only cares about the environment sets
	// nothing here.
	secretOpts config.SecretOptions
	now        func() time.Time
	// goos is runtime.GOOS, overridable so the isolated-mode-on-Windows refusal
	// can be exercised on any machine.
	goos string
	// ctx is cancelled by SIGINT or SIGTERM.
	ctx context.Context

	// probes are doctor's seams onto lore, Telegram and the endpoints. Zero-valued
	// fields fall back to the real ones; tests fill them with fakes.
	probes probes

	// supervisors builds the thing `run` runs. Injected in tests so that argument
	// handling and the startup summary can be checked without a bot token.
	supervisors supervisorFactory
}

func newEnv(ctx context.Context) *env {
	return &env{
		stdin:     os.Stdin,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		lookupEnv: os.LookupEnv,
		now:       time.Now,
		goos:      runtime.GOOS,
		ctx:       ctx,
	}
}

func (e *env) context() context.Context {
	if e.ctx == nil {
		return context.Background()
	}
	return e.ctx
}

func (e *env) clock() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
}

func (e *env) env() config.LookupEnvFunc {
	if e.lookupEnv == nil {
		return os.LookupEnv
	}
	return e.lookupEnv
}

// secrets builds the resolver that turns a SecretRef into a value.
//
// A fresh resolver each time is deliberate and matches what internal/config does:
// it caches nothing, so a rotated credential file is picked up without a restart,
// and no resolved value is held anywhere in this process longer than the call that
// asked for it.
//
// Nothing in this package reads a `_env` variable itself any more. The order —
// `_file`, then `_env`, then the systemd credential — and the rule that stating two
// sources is an error rather than a precedence win are internal/config's, and a
// second implementation of them here is how a household ends up passing validation
// and then finding no token where it is needed.
func (e *env) secrets() *config.Secrets {
	opts := e.secretOpts
	if opts.LookupEnv == nil {
		opts.LookupEnv = e.env()
	}
	return config.NewSecrets(opts)
}

func (e *env) os() string {
	if e.goos == "" {
		return runtime.GOOS
	}
	return e.goos
}

// dispatch routes one command line. It is the whole of main's logic.
func dispatch(e *env, args []string) int {
	if len(args) == 0 {
		usage(e.stderr)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return cmdRun(e, args[1:])
	case "setup":
		return cmdSetup(e, args[1:])
	case "dashboard":
		return cmdDashboard(e, args[1:])
	case "setup-token":
		return cmdSetupToken(e, args[1:])
	case "invite":
		return cmdInvite(e, args[1:])
	case "revoke":
		return cmdRevoke(e, args[1:])
	case "doctor":
		return cmdDoctor(e, args[1:])
	case "update":
		return cmdUpdate(e, args[1:])
	case "version":
		return cmdVersion(e, args[1:])
	case "help", "-h", "--help":
		usage(e.stdout)
		return exitOK
	default:
		fmt.Fprintf(e.stderr, "kenward: unknown command %q\n\n", args[0])
		usage(e.stderr)
		return exitUsage
	}
}

const usageText = `kenward — a household assistant that remembers.

Usage:
  kenward <command> [flags]

Commands:
  run       Run the node. This is what the container entrypoint and the systemd
            unit call.
  setup     First-run wizard at the terminal. Writes kenward.yaml and is one of
            the two places the mode is chosen.
  dashboard Serve the admin dashboard on its own, without running the household.
            This is how a first run is done in a browser: it works with no
            configuration at all, and prints a single-use setup token.
  setup-token
            Reissue that token. Only while there is no admin account.
  invite    Mint a single-use claim code for a member.
  revoke    Unbind a member's Telegram account.
  doctor    Check that this node works and report what it is actually doing.
  update    Check for, or apply, a signed update.
  version   Print the build.

Run "kenward <command> --help" for a command's own flags.

Exit codes: 0 success, 1 runtime failure, 2 configuration or usage error.
`

func usage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

// newFlagSet builds a FlagSet that reports parse errors through e.stderr and never
// calls os.Exit, so that a bad flag is an exit code this package chooses rather than
// one the flag package takes on its behalf.
func newFlagSet(e *env, name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "Usage: %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags runs fs over args and maps the flag package's outcomes onto exit codes.
// The bool reports whether the caller should carry on.
func parseFlags(e *env, fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK, false
		}
		return exitUsage, false
	}
	return exitOK, true
}

// parseWithPositionals parses flags that may appear on either side of a positional
// argument, and returns the positionals.
//
// The flag package stops at the first non-flag argument, so `kenward revoke david
// --config /etc/kenward/kenward.yaml` would otherwise read --config as a second
// member id and refuse. Operators write flags after the subject all the time, and a
// command that refuses a reasonable-looking line while naming the wrong problem is
// worse than one extra loop here.
func parseWithPositionals(e *env, fs *flag.FlagSet, args []string) ([]string, int, bool) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			if err == flag.ErrHelp {
				return nil, exitOK, false
			}
			return nil, exitUsage, false
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, exitOK, true
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

// errorf writes an operator-facing error to stderr. Errors go to stderr so that
// anything a script might parse can have stdout to itself.
func (e *env) errorf(format string, args ...any) {
	fmt.Fprintf(e.stderr, "kenward: "+format+"\n", args...)
}

// printf writes to stdout.
func (e *env) printf(format string, args ...any) {
	fmt.Fprintf(e.stdout, format, args...)
}

// indent prefixes every line of s with pad. It is used for lists under a heading,
// never for the privacy statement, which is printed exactly as internal/privacy
// wrote it.
func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == "" {
			continue
		}
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
