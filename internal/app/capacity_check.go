package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/moby/moby/client"
)

// hostSize is the Docker host a worker builds and runs previews on. A zero
// field is unknown and not checked.
type hostSize struct {
	memory, disk int64 // bytes
	cpus         int
}

// platformReserve is the memory Previa itself needs beside builds and
// previews: PostgreSQL, the server, the worker, the router and the proxy.
const platformReserve = 1 << 30

// capacityWarnings says, in plain words, where the configured budgets exceed
// the host. Exceeding them does not stop the worker (the operator may know
// better), but on a small server it means builds and previews running out of
// memory, or waiting for disk forever.
func capacityWarnings(cfg config.WorkerConfig, h hostSize) []string {
	var out []string
	builds := int64(max(cfg.MaxConcurrentBuilds, 1))
	if h.memory > 0 {
		previews := int64(cfg.PreviewMemoryBudget)
		if previews <= 0 {
			previews = h.memory * 60 / 100 // the runtime's default budget
		}
		if need := builds*int64(cfg.BuildMemory) + previews + platformReserve; need > h.memory {
			out = append(out, fmt.Sprintf("memory: %d build(s) of BUILD_MEMORY %s, previews' %s and about 1 GiB for Previa need %s, but this host has %s; "+
				"lower MAX_CONCURRENT_BUILDS, BUILD_MEMORY or PREVIEW_MEMORY_BUDGET",
				builds, cfg.BuildMemory, config.ByteSize(previews), config.ByteSize(need), config.ByteSize(h.memory)))
		}
	}
	if h.cpus > 0 && float64(builds)*cfg.BuildCPUs > float64(h.cpus) {
		out = append(out, fmt.Sprintf("CPU: %d build(s) of BUILD_CPUS %g can use more than this host's %d CPUs; previews and the dashboard slow down while they run",
			builds, cfg.BuildCPUs, h.cpus))
	}
	if h.disk > 0 {
		if need := int64(cfg.BuildMinFreeDisk) + builds*int64(cfg.BuildDiskBudget) + int64(cfg.BuildCacheBudget); need > h.disk {
			out = append(out, fmt.Sprintf("disk: BUILD_MIN_FREE_DISK %s, %d build(s) of BUILD_DISK_BUDGET %s and BUILD_CACHE_BUDGET %s need %s, but Docker's disk is %s; "+
				"builds would wait for disk space; lower them",
				cfg.BuildMinFreeDisk, builds, cfg.BuildDiskBudget, cfg.BuildCacheBudget, config.ByteSize(need), config.ByteSize(h.disk)))
		}
	}
	return out
}

// warnCapacity logs every budget that exceeds the host, once at startup. It
// never fails: a host it cannot measure is simply not checked.
func warnCapacity(ctx context.Context, docker *client.Client, cfg config.WorkerConfig, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var h hostSize
	if info, err := docker.Info(ctx, client.InfoOptions{}); err == nil {
		h.memory, h.cpus = info.Info.MemTotal, info.Info.NCPU
	}
	// The router's root file system sits on Docker's disk; its image has df.
	if result, err := dockerx.Run(ctx, docker, cfg.RouterContainer, "df", "-Pk", "/"); err == nil && result.ExitCode == 0 {
		h.disk = dfTotal(result.Stdout)
	}
	for _, warning := range capacityWarnings(cfg, h) {
		logger.Warn("capacity settings exceed this host (docs/25-production-deployment.md#starter-settings-for-a-small-host)", "detail", warning)
	}
}

// dfTotal reads the total kilobytes from `df -Pk` output, 0 if unreadable.
func dfTotal(out string) int64 {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 2 {
		return 0
	}
	kb, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || kb <= 0 {
		return 0
	}
	return kb << 10
}
