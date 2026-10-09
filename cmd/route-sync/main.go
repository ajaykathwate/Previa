// Command previa-route-sync writes the accepted route table from PostgreSQL
// to the router's dynamic directory, as the worker does at startup.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ajaykathwatee/previa/internal/app"
)

func main() {
	os.Exit(run())
}

// run returns the exit code; its deferred cleanup runs before the process
// exits.
func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.RunRouteSync(ctx, os.Stdout); err != nil {
		slog.Error("route table sync failed", "error", err)
		return 1
	}
	return 0
}
