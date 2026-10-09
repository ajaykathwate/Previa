// Command previa-housekeeping runs housekeeping as a dry run: it logs every
// decision (expiry, orphaned resources, pruning) and changes nothing.
//
//	previa-housekeeping [-workspaces DIR]
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
	os.Exit(run(*workspaces))
}

// run returns the exit code; its deferred cleanup runs before the process
// exits.
func run(workspaces string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.RunHousekeepingDryRun(ctx, os.Stdout, workspaces); err != nil {
		slog.Error("housekeeping dry run failed", "error", err)
		return 1
	}
	return 0
}
