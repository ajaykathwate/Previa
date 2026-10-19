package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ajaykathwatee/previa/internal/app"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/errortracking"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/logging"
	"github.com/ajaykathwatee/previa/internal/platform/version"
)

// main runs the server, or one of its subcommands:
//
//	server check-config  validate the environment and exit (deploy gate)
//	server healthcheck   exit 0 when the running process is ready (container health check)
//	server sentry-test   send one test error to Sentry and wait for it (deploy check)
//	server journal-*     private snapshot/export/ack/replay deployment commands
func main() {
	if len(os.Args) > 1 {
		os.Exit(command(os.Args[1]))
	}
	os.Exit(run())
}

// run runs the server until it is told to stop and returns the exit code; its
// deferred cleanup runs before the process exits.
func run() int {
	// Warnings and errors also go to Sentry when SENTRY_DSN is set (docs/25).
	reporter, reportErr := errortracking.Start(errortracking.FromEnv("server", version.Version))
	defer reporter.Flush(5 * time.Second)
	logger, err := logging.New(os.Stdout, os.Getenv("LOG_LEVEL"), reporter.Handlers()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if reportErr != nil {
		logger.Error("error reporting is off", "error", reportErr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.RunServer(ctx, logger); err != nil {
		logger.Error("server stopped", "error", err)
		return 1
	}
	return 0
}

func command(name string) int {
	switch name {
	case "journal-export", "journal-ack", "journal-replay", "journal-snapshot":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := app.JournalCommand(ctx, name, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, logging.RedactString(err.Error()))
			return 1
		}
		return 0
	case "check-config":
		if _, err := config.LoadServer(); err != nil {
			fmt.Fprintln(os.Stderr, logging.RedactString(err.Error()))
			return 1
		}
		if err := errortracking.Check(errortracking.FromEnv("server", version.Version)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("configuration is valid")
		return 0
	case "healthcheck":
		addr := os.Getenv("INTERNAL_ADDR")
		if addr == "" {
			addr = "127.0.0.1:9090"
		}
		if err := httpx.Probe(addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "sentry-test":
		if err := errortracking.Test(errortracking.FromEnv("server", version.Version)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("sent a test error to Sentry: it appears under Issues as \"Sentry test from previa-server\"")
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: server [check-config|healthcheck|sentry-test|journal-snapshot|journal-export|journal-ack|journal-replay]")
		return 2
	}
}
