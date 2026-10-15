package app

import (
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/config"
)

func workerSizing(builds int, memory, disk, cache, free config.ByteSize, cpus float64) config.WorkerConfig {
	var cfg config.WorkerConfig
	cfg.MaxConcurrentBuilds = builds
	cfg.BuildMemory, cfg.BuildDiskBudget, cfg.BuildCacheBudget, cfg.BuildMinFreeDisk, cfg.BuildCPUs = memory, disk, cache, free, cpus
	return cfg
}

// The defaults on the deployment guide's 8 GB / 4 vCPU / 75 GB VPS exceed it
// three ways; the guide's VPS sizing fits.
func TestCapacityWarningsOnTheDeploymentVPS(t *testing.T) {
	const gib = 1 << 30
	vps := hostSize{memory: 8 * gib, disk: 75 * gib, cpus: 4}
	defaults := capacityWarnings(workerSizing(2, 4*gib, 20*gib, 40*gib, 15*gib, 2), vps)
	if len(defaults) != 2 || !strings.HasPrefix(defaults[0], "memory:") || !strings.HasPrefix(defaults[1], "disk:") {
		t.Fatalf("defaults on the VPS must warn about memory and disk, got %q", defaults)
	}
	sized := workerSizing(1, 2*gib, 10*gib, 10*gib, 10*gib, 2)
	sized.PreviewMemoryBudget = 4 * gib
	if warnings := capacityWarnings(sized, vps); len(warnings) != 0 {
		t.Fatalf("the guide's VPS sizing fits, got %q", warnings)
	}
	if warnings := capacityWarnings(workerSizing(4, gib, gib, gib, gib, 2), hostSize{cpus: 4}); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "CPU:") {
		t.Fatalf("four 2-CPU builds on 4 CPUs warn, got %q", warnings)
	}
	if warnings := capacityWarnings(workerSizing(9, 9*gib, 9*gib, 9*gib, 9*gib, 9), hostSize{}); len(warnings) != 0 {
		t.Fatal("an unmeasured host is not checked")
	}
	if dfTotal("Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 78643200 1000 77000000 1% /\n") != 78643200<<10 {
		t.Fatal("df total misread")
	}
}
