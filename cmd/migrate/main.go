// Command migrate applies Previa's embedded schema migrations and River's,
// for release images: migrate up | migrate status.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ajaykathwatee/previa/internal/app"
	"github.com/ajaykathwatee/previa/internal/platform/logging"
)

func main() {
	os.Exit(run())
}

// run returns the exit code; its deferred cleanup runs before the process
// exits.
func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.RunMigrations(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", logging.RedactString(err.Error()))
		return 1
	}
	return 0
}
