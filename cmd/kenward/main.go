// Command kenward is the household assistant's single binary.
//
// The command surface is specified in docs/CLI.md and is deliberately small: run,
// setup, invite, revoke, doctor, update, version. A household assistant that needs
// a manual is one that does not get installed.
//
// main is a thin dispatcher on purpose. Everything a test would want to assert —
// argument parsing, exit codes, rendered output — lives in functions taking an
// explicit environment, because a `main` package that does its work in main() is a
// package whose behaviour is only ever checked by running it.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// SIGINT and SIGTERM cancel this context. `run` turns that into a drain;
	// every other command simply stops.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := newEnv(ctx)
	os.Exit(dispatch(e, os.Args[1:]))
}
