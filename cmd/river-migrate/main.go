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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.RunRiverMigration(ctx, os.Args[1:], os.Stdout); err != nil {
		slog.Error("River migration failed", "error", err)
		os.Exit(1)
	}
}
