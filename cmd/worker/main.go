package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ajaykathwatee/previa/internal/app"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/logging"
)

// main runs the worker, or one of its subcommands:
//
//	worker check-config  validate the environment and exit (deploy gate)
//	worker healthcheck   exit 0 when the running process is ready (container health check)
func main() {
	if len(os.Args) > 1 {
		os.Exit(command(os.Args[1]))
	}
	logger, err := logging.New(os.Stdout, os.Getenv("LOG_LEVEL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.RunWorker(ctx, logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func command(name string) int {
	switch name {
	case "check-config":
		if _, err := config.LoadWorker(); err != nil {
			fmt.Fprintln(os.Stderr, logging.RedactString(err.Error()))
			return 1
		}
		fmt.Println("configuration is valid")
		return 0
	case "healthcheck":
		addr := os.Getenv("WORKER_INTERNAL_ADDR")
		if addr == "" {
			addr = "127.0.0.1:9091"
		}
		if err := httpx.Probe(addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: worker [check-config|healthcheck]")
		return 2
	}
}
