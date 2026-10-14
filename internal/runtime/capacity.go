package runtime

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Locker serializes preview admission across workers, from the capacity
// check until the candidate exists with its labels, so two candidates never
// both take the last free capacity. *AdmissionLock implements it.
type Locker interface {
	Lock(ctx context.Context) (unlock func(), err error)
}

// usage is what previews already hold: running, restarting or just created
// containers, counted by the limits on their labels. During a swap the active
// preview and its candidate both count, and a container counts until its
// removal: capacity is released only by verified cleanup.
type usage struct {
	cpuMillis, memory int64
	// repoPRs and accountPRs are the repository's and the account's PRs that
	// hold a preview or candidate on this instance. The limits count PRs, so a
	// PR's swap never competes with its own active preview.
	repoPRs, accountPRs map[string]bool
}

// budget is what previews may hold together.
type budget struct {
	cpuMillis, memory int64
	perRepo           int
	perAccount        int // 0: no limit
}

// smallestPreview is the least a preview can need (the small tier); the
// advisory pre-admission check asks whether even that fits.
const smallestCPUMillis, smallestMemory = 500, 512 << 20

// budget derives the preview budget: the operator's values, or 60% of the
// Docker host's memory and 75% of its CPUs, leaving the rest for the platform.
func (r *Runtime) budget(ctx context.Context) (budget, error) {
	b := budget{memory: r.policy.MemoryBudget, cpuMillis: int64(r.policy.CPUBudget * 1000), perRepo: r.policy.MaxPerRepo, perAccount: r.policy.MaxPerAccount}
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

// usage sums the limits recorded on candidate containers' labels: apps and
// their managed services, setup and processes. The host budget counts every
// Previa preview on the Docker host, whichever instance created it; the
// repository count only this instance's. A stopped app (a sleeping or
// crashed preview) uses no memory or CPU but is still a preview, so it
// counts towards the repository and account limits.
func (r *Runtime) usage(ctx context.Context, t Target) (usage, error) {
	listed, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label", dockerx.LabelManaged+"=true")})
	if err != nil {
		return usage{}, fmt.Errorf("list running previews: %w", err)
	}
	u := usage{repoPRs: map[string]bool{}, accountPRs: map[string]bool{}}
	legacyWorkspaces := make(map[string]int64)
	for _, c := range listed.Items {
		if !slices.Contains(dockerx.CandidateKinds, c.Labels[dockerx.LabelKind]) {
			continue
		}
		running := c.State == container.StateRunning || c.State == container.StateRestarting || c.State == container.StateCreated
		stoppedApp := c.State == container.StateExited && c.Labels[dockerx.LabelKind] == dockerx.KindPreview
		if !running && !stoppedApp {
			continue
		}
		if running {
			cpu, _ := strconv.ParseInt(c.Labels[dockerx.LabelCPUMillis], 10, 64)
			memory, _ := strconv.ParseInt(c.Labels[dockerx.LabelMemory], 10, 64)
			u.cpuMillis += cpu
			u.memory += memory
		}
		if c.Labels[dockerx.LabelInstance] == r.instance && c.Labels[dockerx.LabelBaseRepo] == strconv.FormatInt(t.BaseRepoID, 10) {
			u.repoPRs[c.Labels[dockerx.LabelPR]] = true
		}
		if c.Labels[dockerx.LabelInstance] != r.instance || t.WorkspaceID <= 0 {
			continue
		}
		workspace, err := r.containerWorkspace(ctx, c.Labels, legacyWorkspaces)
		if err != nil {
			return usage{}, err
		}
		if workspace == t.WorkspaceID {
			u.accountPRs[c.Labels[dockerx.LabelPR]] = true
		}
	}
	return u, nil
}

// containerWorkspace reconciles pre-workspace container labels through stable PR
// identity; legacy account labels contained user IDs and must not be trusted.
func (r *Runtime) containerWorkspace(ctx context.Context, labels map[string]string, cache map[string]int64) (int64, error) {
	if id := labels[dockerx.LabelWorkspace]; id != "" {
		v, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("read workspace label: %w", err)
		}
		return v, nil
	}
	pr := labels[dockerx.LabelPR]
	if id, ok := cache[pr]; ok {
		return id, nil
	}
	id, err := strconv.ParseInt(pr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read legacy preview identity: %w", err)
	}
	scope, err := r.targets.AdmissionScope(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("resolve legacy preview workspace: %w", err)
	}
	cache[pr] = scope.WorkspaceID
	return scope.WorkspaceID, nil
}

// admit refuses a candidate that would exceed the per-repository count or the
// host budget. The refusal is retryable: capacity frees up as previews end.
func admit(u usage, b budget, pullRequestID, cpuMillis, memory int64) error {
	pr := strconv.FormatInt(pullRequestID, 10)
	others := len(u.repoPRs)
	if u.repoPRs[pr] {
		others--
	}
	accountOthers := len(u.accountPRs)
	if u.accountPRs[pr] {
		accountOthers--
	}
	switch {
	case others >= b.perRepo:
		return failure(CodeCapacity, fmt.Sprintf("This repository already has %d previews, its limit.", others), true, nil)
	case b.perAccount > 0 && accountOthers >= b.perAccount:
		return failure(CodeCapacity, fmt.Sprintf("This workspace already has %d previews on this server, its limit. Tear one down, or this starts when one ends.", accountOthers), true, nil)
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
	u, err := r.usage(ctx, t)
	if err != nil {
		return failure(CodeUnavailable, "Preview capacity could not be checked.", true, err)
	}
	return admit(u, b, t.PullRequestID, cpuMillis, memory)
}

// Admit implements environments.Headroom: before a build is admitted it asks
// whether even the smallest preview would fit, so a request that cannot run
// waits queued instead of building an image it cannot start. It takes no
// lock; startCandidate checks again atomically.
func (r *Runtime) Admit(ctx context.Context, request environments.Request) error {
	if a, ok := r.targets.(interface {
		AuthorizeRequest(context.Context, int64) error
	}); ok {
		if err := a.AuthorizeRequest(ctx, request.PullRequestID); err != nil {
			return err
		}
	}
	scope, err := r.targets.AdmissionScope(ctx, request.PullRequestID)
	if err != nil {
		return err
	}
	err = r.checkCapacity(ctx, scope, smallestCPUMillis, smallestMemory)
	if CodeOf(err) == CodeCapacity {
		return &environments.WaitingError{Reason: environments.WaitCapacity, Err: err}
	}
	return err
}

// AdmissionLock is a Locker backed by a PostgreSQL session advisory lock. A
// worker that crashes while holding it loses its connection, which releases
// the lock; nothing is left to reconcile.
type AdmissionLock struct{ pool *pgxpool.Pool }

func NewAdmissionLock(pool *pgxpool.Pool) *AdmissionLock { return &AdmissionLock{pool: pool} }

// The key includes this database's instance: advisory locks are shared by
// every schema of a PostgreSQL database.
const admissionLockKey = "(SELECT hashtext('previa.preview_admission.' || instance) FROM platform_instance)"

func (l *AdmissionLock) Lock(ctx context.Context) (func(), error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for preview admission: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock("+admissionLockKey+")"); err != nil {
		// A cancelled wait may still have been granted; closing the
		// connection releases the lock either way.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.Conn().Close(closeCtx)
		cancel()
		conn.Release()
		return nil, fmt.Errorf("lock preview admission: %w", err)
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock("+admissionLockKey+")"); err != nil {
			// Never return a connection that may still hold the lock.
			_ = conn.Conn().Close(ctx)
		}
		conn.Release()
	}, nil
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
