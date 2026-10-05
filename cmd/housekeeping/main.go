package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ajaykathwatee/previa/internal/app"
)

func main() {
	workspaces := flag.String("workspaces", "", "absolute source workspace directory to inspect (optional)")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.RunHousekeepingDryRun(ctx, os.Stdout, *workspaces); err != nil {
		slog.Error("housekeeping dry run failed", "error", err)
		os.Exit(1)
	}
}
