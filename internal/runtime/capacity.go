package runtime

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// usage is what previews already hold: running, restarting or just created.
type usage struct {
	cpuMillis, memory int64
	inRepo            int
}

// budget is what previews may hold together.
type budget struct {
	cpuMillis, memory int64
	perRepo           int
}

// budget derives the preview budget: the operator's values, or 60% of the
// Docker host's memory and 75% of its CPUs, leaving the rest for the platform.
func (r *Runtime) budget(ctx context.Context) (budget, error) {
	b := budget{memory: r.policy.MemoryBudget, cpuMillis: int64(r.policy.CPUBudget * 1000), perRepo: r.policy.MaxPerRepo}
	if b.memory > 0 && b.cpuMillis > 0 {
		return b, nil
	}
	info, err := r.docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		return budget{}, fmt.Errorf("read Docker host resources: %w", err)
	}
	if b.memory <= 0 {
		b.memory = info.Info.MemTotal * 60 / 100
	}
	if b.cpuMillis <= 0 {
		b.cpuMillis = int64(info.Info.NCPU) * 750
	}
	return b, nil
}

// usage sums the limits recorded on preview containers' labels.
func (r *Runtime) usage(ctx context.Context, baseRepoID int64) (usage, error) {
	listed, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label",
		dockerx.LabelManaged+"=true", dockerx.LabelKind+"="+dockerx.KindPreview)})
	if err != nil {
		return usage{}, fmt.Errorf("list running previews: %w", err)
	}
	var u usage
	for _, c := range listed.Items {
		if c.State != container.StateRunning && c.State != container.StateRestarting && c.State != container.StateCreated {
			continue
		}
		cpu, _ := strconv.ParseInt(c.Labels[dockerx.LabelCPUMillis], 10, 64)
		memory, _ := strconv.ParseInt(c.Labels[dockerx.LabelMemory], 10, 64)
		u.cpuMillis += cpu
		u.memory += memory
		if c.Labels[dockerx.LabelBaseRepo] == strconv.FormatInt(baseRepoID, 10) {
			u.inRepo++
		}
	}
	return u, nil
}

// admit refuses a candidate that would exceed the per-repository count or the
// host budget. The refusal is retryable: capacity frees up as previews end.
func admit(u usage, b budget, cpuMillis, memory int64) error {
	switch {
	case u.inRepo >= b.perRepo:
		return failure(CodeCapacity, fmt.Sprintf("This repository already runs %d previews, its limit.", u.inRepo), true, nil)
	case u.memory+memory > b.memory:
		return failure(CodeCapacity, fmt.Sprintf("Previews would need %s of memory together; the host allows %s.", humanBytes(u.memory+memory), humanBytes(b.memory)), true, nil)
	case u.cpuMillis+cpuMillis > b.cpuMillis:
		return failure(CodeCapacity, fmt.Sprintf("Previews would need %.2f CPUs together; the host allows %.2f.", float64(u.cpuMillis+cpuMillis)/1000, float64(b.cpuMillis)/1000), true, nil)
	}
	return nil
}

func (r *Runtime) checkCapacity(ctx context.Context, t Target, cpuMillis, memory int64) error {
	b, err := r.budget(ctx)
	if err != nil {
		return failure(CodeUnavailable, "Preview capacity could not be checked.", true, err)
	}
	u, err := r.usage(ctx, t.BaseRepoID)
	if err != nil {
		return failure(CodeUnavailable, "Preview capacity could not be checked.", true, err)
	}
	return admit(u, b, cpuMillis, memory)
}

// humanBytes renders a size with one decimal in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n), "B"
	for _, s := range []string{"KiB", "MiB", "GiB", "TiB"} {
		if value < unit {
			break
		}
		value, suffix = value/unit, s
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
