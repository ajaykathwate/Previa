// Command previa-egress is the forward proxy that is the only route out of
// Previa's build and preview networks, and the origin of readiness probes into
// previews. It runs in its own container (see internal/platform/egress):
//
//	previa-egress [serve] [-listen :3128]
//	previa-egress probe -target IP:PORT [-path /health] [-timeout 2s]
//
// probe exits 0 when the target is ready, 1 when it is not (printing why) and
// 2 for invalid arguments.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/egress"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "probe" {
		os.Exit(probe(args[1:]))
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	serve(args)
}

func probe(args []string) int {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	target := flags.String("target", "", "IP address and port of the preview")
	path := flags.String("path", "", "HTTP health path; empty checks that the port accepts connections")
	timeout := flags.Duration("timeout", 2*time.Second, "probe timeout")
	if err := flags.Parse(args); err != nil || *target == "" || *timeout <= 0 {
		return 2
	}
	if err := egress.Probe(context.Background(), *target, *path, *timeout); err != nil {
		fmt.Println(err.Error())
		return 1
	}
	fmt.Println("ready")
	return 0
}

func serve(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := flags.String("listen", ":"+egress.Port, "address to listen on")
	_ = flags.Parse(args)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	server := &http.Server{
		Addr:              *listen,
		Handler:           egress.NewProxy(logger),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("egress proxy listening", "address", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("egress proxy stopped", "error", err)
		os.Exit(1)
	}
}
