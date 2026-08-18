package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/dashboard"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// cmdDashboard serves the admin dashboard on its own, without running the household.
//
// It exists because the first run has to be reachable and `kenward run` cannot serve it:
// run refuses to start without a configuration, and the whole point of the wizard is
// that there is not one yet. So this is the command an installer points somebody at, and
// it is the only one that works on a machine with nothing on it.
//
// Once a household is configured, `kenward run` serves the same dashboard from inside
// the node — same package, same routes, same account — and this command is for the times
// something is wrong enough that the node will not start.
func cmdDashboard(e *env, args []string) int {
	fs := newFlagSet(e, "dashboard", "kenward dashboard [--config PATH] [--data-dir PATH] [--bind ADDR]")
	configPath := fs.String("config", "", "path to kenward.yaml; it does not have to exist yet")
	dataDir := fs.String("data-dir", "", "where the admin account lives (default: the configuration's data_dir, then the per-OS state location)")
	bind := fs.String("bind", "", "host:port to listen on (default: "+config.DefaultDashboardBind+", or whatever the configuration says)")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		e.errorf("dashboard takes no positional arguments; got %q", fs.Arg(0))
		return exitUsage
	}

	path := resolveConfigPath(e, *configPath)
	dash, dir, err := dashboardSettings(e, path, resolveDataDir(e, *dataDir))
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}
	// A --bind is an override of the file, and it is deliberately not allowed to make
	// a claim: binding somewhere that is not loopback while the file says loopback is
	// exactly the disagreement config.validateDashboard refuses, so the exposure is
	// recomputed from the address rather than carried over.
	if *bind != "" {
		dash.Bind = *bind
		if !dash.Loopback() && dash.ExposureOrDefault() == config.ExposureLoopback {
			e.errorf("--bind %s is not a loopback address, and this configuration says the dashboard is\n"+
				"loopback-only. Choose the exposure under Access in the dashboard itself, on\n"+
				"loopback, where the account that protects it already exists — rather than opening\n"+
				"the port first and deciding afterwards.", *bind)
			return exitUsage
		}
	}
	dash.Enabled = true

	logger := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, err := dashboard.New(dashboardDeps(e, path, dir, logger), dash)
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}
	if err := srv.Listen(); err != nil {
		e.errorf("%v", err)
		return exitFailure
	}

	token, err := srv.SetupTokenIfNeeded()
	if err != nil {
		e.errorf("%v", err)
		return exitFailure
	}
	fmt.Fprint(e.stdout, renderDashboardBanner(srv.URL(), token))

	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()

	select {
	case err := <-done:
		if err != nil {
			e.errorf("%v", err)
			return exitFailure
		}
	case <-e.context().Done():
		ctx, cancel := context.WithTimeout(context.WithoutCancel(e.context()), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			e.errorf("shutdown was not clean: %v", err)
			return exitFailure
		}
	}
	return exitOK
}

// cmdSetupToken reissues the first-run token.
//
// It is a separate command because the token has a thirty-minute life and the process
// that printed the first one may have been started hours ago, or by a service manager
// whose output nobody kept. Reissuing voids whatever was outstanding: there is one token.
func cmdSetupToken(e *env, args []string) int {
	fs := newFlagSet(e, "setup-token", "kenward setup-token [--config PATH] [--data-dir PATH]")
	configPath := fs.String("config", "", "path to kenward.yaml")
	dataDir := fs.String("data-dir", "", "override the data directory")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		e.errorf("setup-token takes no positional arguments; got %q", fs.Arg(0))
		return exitUsage
	}

	path := resolveConfigPath(e, *configPath)
	_, dir, err := dashboardSettings(e, path, resolveDataDir(e, *dataDir))
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}

	if dashboard.NewAdminStore(dir).Exists() {
		// Not an error the operator can act on by trying again, and not a thing to
		// do quietly: issuing a token for a household that has an account would be
		// minting a way past the password.
		e.errorf("this household already has an admin account, so there is nothing a setup token\n" +
			"could be for. Sign in with the password. If it is lost, delete dashboard/admin.json\n" +
			"under the data directory from a shell on this machine and run setup again.")
		return exitUsage
	}

	token, err := dashboard.NewSetupTokenStore(dir).Issue(e.now())
	if err != nil {
		e.errorf("%v", err)
		return exitFailure
	}
	fmt.Fprint(e.stdout, renderSetupToken(token))
	return exitOK
}

// dashboardSettings resolves the dashboard block and the data directory, from a
// configuration that may not exist yet.
//
// A missing configuration is the ordinary first-run case and not a fault: the defaults
// are loopback, off, and the platform's own state location, which is exactly what a
// machine with nothing on it should get.
func dashboardSettings(e *env, path, dataDirOverride string) (config.DashboardConfig, string, error) {
	dash := config.DashboardConfig{}
	dir := dataDirOverride

	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if dir == "" {
			dir = config.DefaultDataDir()
		}
		return dash, dir, nil
	case err != nil:
		return dash, "", err
	}
	defer f.Close()

	cfg, err := config.Decode(f)
	if err != nil {
		// A configuration too broken to decode is one somebody wants the dashboard
		// for. Serve it against the defaults and let the overview say what is wrong.
		if dir == "" {
			dir = config.DefaultDataDir()
		}
		return dash, dir, nil
	}
	if dir == "" {
		dir = cfg.DataDir
	}
	if dir == "" {
		dir = config.DefaultDataDir()
	}
	return cfg.Dashboard, dir, nil
}

// dashboardDeps wires the dashboard onto this binary's own enrolment and memory paths.
//
// Minting and revoking are handed over as functions rather than reimplemented inside
// internal/dashboard, because both are mode-dependent and both fail silently when the
// mode-dependent half is skipped: a code that never reaches a member's pod, a revocation
// the pod never reads. Those two paths are written once, here, and tested here.
func dashboardDeps(e *env, path, dataDir string, logger *slog.Logger) dashboard.Deps {
	return dashboard.Deps{
		ConfigPath: path,
		DataDir:    dataDir,
		Now:        e.clock,
		Logger:     logger,
		Lore: func(ctx context.Context) (dashboard.SpaceClient, error) {
			return memory.NewClient(memory.Config{})
		},
		MintInvite: func(ctx context.Context, cfg *config.Config, id domain.MemberID, name string, ttl time.Duration) (string, error) {
			return mintClaimCode(ctx, e, cfg, id, name, ttl)
		},
		Revoke: func(ctx context.Context, cfg *config.Config, id domain.MemberID) error {
			_, _, err := revokeMember(ctx, e, cfg, path, id)
			return err
		},
	}
}

// renderDashboardBanner is what the process prints on the way up.
//
// The token is on stdout, once, and nowhere else: it is not logged, because a log is a
// file, and a live credential in a file that a service manager rotates into somebody's
// journal is the thing loopback-by-default exists to avoid needing to worry about.
func renderDashboardBanner(url, token string) string {
	if token == "" {
		return fmt.Sprintf("kenward dashboard listening on %s\n\nSign in with the admin password.\n", url)
	}
	return fmt.Sprintf("kenward dashboard listening on %s\n\n%s", url, renderSetupToken(token))
}

func renderSetupToken(token string) string {
	return fmt.Sprintf(`This household has no admin account yet. Open the dashboard and paste this token:

    %s

It works once and expires in %s. Reissue with `+"`kenward setup-token`"+`.
Setup happens on loopback: nothing on your network can reach that page.
`, token, dashboard.SetupTokenTTL)
}
