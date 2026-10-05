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
	if err := app.RunRouteSync(ctx, os.Stdout); err != nil {
		slog.Error("route table sync failed", "error", err)
		os.Exit(1)
	}
}
