package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// Environment variables the container image sets (see Dockerfile). They are the
// defaults for --config and --data-dir so that `docker run image` works with no
// arguments at all, and an operator overriding one on the command line still wins.
const (
	envConfigPath = "KENWARD_CONFIG"
	envDataDir    = "KENWARD_DATA_DIR"
)

// resolveConfigPath implements docs/CLI.md's rule: the flag, then KENWARD_CONFIG,
// then kenward.yaml in the working directory, then the per-OS config location.
//
// The working directory comes before the per-OS location because that is where
// `kenward setup` writes by default, and somebody who has just run setup and then
// run kenward in the same shell must not be told there is no configuration.
func resolveConfigPath(e *env, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v, ok := e.env()(envConfigPath); ok && v != "" {
		return v
	}
	if _, err := os.Stat(setup.DefaultConfigFileName); err == nil {
		return setup.DefaultConfigFileName
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "kenward", setup.DefaultConfigFileName)
	}
	return setup.DefaultConfigFileName
}

// resolveDataDir returns the data directory override, or "" for whatever the
// configuration itself says. Empty is meaningful: config.ApplyDefaults already
// resolves it to the per-OS state location, and second-guessing that here would put
// two answers in the binary.
func resolveDataDir(e *env, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v, ok := e.env()(envDataDir); ok && v != "" {
		return v
	}
	return ""
}

// loadConfig reads, merges and validates a configuration, honouring a data
// directory override.
//
// It mirrors config.LoadWithEnv's order — decode, merge recorded enrolments,
// validate — with the override applied in the middle, because the state file lives
// under the data directory and reading it from the configured location before
// applying --data-dir would merge the wrong household's bindings.
func loadConfig(path, dataDir string, secrets *config.Secrets) (*config.Config, error) {
	return loadConfigForUnit(path, dataDir, secrets, config.UnitScope{})
}

// loadConfigForUnit is loadConfig for a process that runs one unit: `run` in a pod, and
// the `doctor` its health check invokes.
//
// The scope reaches only as far as which secrets have to resolve. A member's pod holds
// its own bot token and no other (D-007), so demanding the household's tokens of it
// would be demanding the one thing the mode forbids — but everything else in the file is
// still checked in full, because it is the same file every other pod reads.
func loadConfigForUnit(path, dataDir string, secrets *config.Secrets, scope config.UnitScope) (*config.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	cfg, err := config.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}

	st, err := config.LoadState(cfg.StatePath())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	mergeErr := cfg.MergeState(st)
	// ValidateForUnit rather than Validate: a household may supply its token as a
	// file or a systemd credential, and judging it against the environment alone
	// would refuse a configuration that is in fact complete.
	validateErr := cfg.ValidateForUnit(secrets, scope)
	if joined := joinValidation(mergeErr, validateErr); joined != nil {
		return cfg, joined
	}
	return cfg, nil
}

// joinValidation folds several validation results into one list, so an operator
// editing a configuration by hand sees every problem in one sitting rather than one
// per restart. It mirrors the unexported helper in internal/config for the same
// reason that package has one.
func joinValidation(errs ...error) error {
	joined := &config.ValidationError{}
	var other []error
	for _, err := range errs {
		if err == nil {
			continue
		}
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			joined.Problems = append(joined.Problems, ve.Problems...)
			joined.MissingEnv = append(joined.MissingEnv, ve.MissingEnv...)
			continue
		}
		other = append(other, err)
	}
	if len(other) > 0 {
		return errors.Join(other...)
	}
	if len(joined.Problems) == 0 && len(joined.MissingEnv) == 0 {
		return nil
	}
	joined.MissingEnv = dedupe(joined.MissingEnv)
	return joined
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// renderConfigError turns a load failure into the operator-facing report.
//
// Every problem is listed, and the missing environment variables are listed by name
// under their own heading with an export line, because "kenward will not start until
// these exist" is the most common way a first run fails and a list is a great deal
// kinder than a validation error. No value is ever printed — only names.
func renderConfigError(path string, err error) string {
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		return fmt.Sprintf("kenward: %v\n", err)
	}

	var b strings.Builder
	if n := len(ve.Problems); n > 0 {
		fmt.Fprintf(&b, "kenward: %s cannot be served (%s):\n", path, plural(n, "problem", "problems"))
		for _, p := range ve.Problems {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	if n := len(ve.MissingEnv); n > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "kenward: %s referenced but not set:\n",
			plural(n, "1 environment variable is", fmt.Sprintf("%d environment variables are", n)))
		for _, name := range ve.MissingEnv {
			fmt.Fprintf(&b, "  - %s\n", name)
		}
		b.WriteString("\nSet every one of them and start again. kenward never starts half-configured:\n")
		b.WriteString("a node missing one member's token would serve everyone else and silently\n")
		b.WriteString("drop that member.\n")
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
