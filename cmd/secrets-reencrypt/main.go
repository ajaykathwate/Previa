// Command previa-secrets-reencrypt moves every repository secret onto the
// active encryption key and reports how many values still use older keys.
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
	if err := app.RunSecretsReencrypt(ctx, os.Stdout); err != nil {
		slog.Error("secret re-encryption failed", "error", err)
		return 1
	}
	return 0
}
