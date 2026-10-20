package environments_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/usage"
)

func TestBurstAdmissionsRespectGlobalAndWorkspaceLimits(t *testing.T) {
	testBurstAdmissions(t, false)
}

func TestBurstAdmissionsRespectFreePlanEntitlements(t *testing.T) {
	testBurstAdmissions(t, true)
}

func testBurstAdmissions(t *testing.T, entitlement bool) {
	t.Helper()
	for _, limits := range []struct {
		global, workspace int
	}{{1, 2}, {2, 1}, {3, 2}, {6, 2}} {
		t.Run(fmt.Sprintf("global%d-workspace%d", limits.global, limits.workspace), func(t *testing.T) {
			f := newFixture(t)
			other, _ := f.secondAccount(t)
			if entitlement {
				workspaceUsage(t, f, limits.workspace)
				f.repo.WithBuildSlots(limits.global)
			} else {
				f.repo.WithBuildSlots(limits.global).WithAccountLimits(env.AccountLimits{BuildsPerAccount: limits.workspace})
			}
			runtime := newGatedRuntime()
			gate := make(chan struct{})
			var requests []env.Request
			for i := range 8 {
				requests = append(requests, f.prEvent(t, int32(100+i), fmt.Sprintf("sha-first-%d", i)))
				requests = append(requests, f.prEventIn(t, other, int32(200+i), fmt.Sprintf("sha-second-%d", i)))
			}
			for _, request := range requests {
				runtime.gates[request.PullRequestID] = gate
			}
			service := newCapacityService(t, f, runtime)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			results := make(chan error, len(requests))
			start := make(chan struct{})
			var workers sync.WaitGroup
			// Always release and join goroutines, including on a failed assertion.
			defer func() { close(gate); cancel(); workers.Wait() }()
			for _, request := range requests {
				workers.Go(func() {
					<-start
					results <- service.Build(ctx, request)
				})
			}
			close(start)
			active := min(limits.global, 2*limits.workspace)
			for range len(requests) - active {
				select {
				case err := <-results:
					var waiting *env.WaitingError
					if !errors.As(err, &waiting) || (waiting.Reason != env.WaitCapacity && waiting.Reason != env.WaitAccount && waiting.Reason != usage.WaitBuilds) {
						t.Fatalf("burst request should wait for capacity: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("burst admissions did not settle within the deadline")
				}
			}
			var running, busiest int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM build_intents WHERE state='running'`).Scan(&running); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(ctx, `SELECT coalesce(max(n),0) FROM (
				SELECT count(*) n FROM build_intents b JOIN pull_requests p ON p.id=b.pull_request_id
				JOIN repos r ON r.id=p.repo_id JOIN installations i ON i.id=r.installation_id
				WHERE b.state='running' GROUP BY i.workspace_id) admitted`).Scan(&busiest); err != nil {
				t.Fatal(err)
			}
			if running != active || busiest > limits.workspace {
				t.Fatalf("admitted %d total and %d in one workspace, want total %d and workspace <=%d", running, busiest, active, limits.workspace)
			}
			for range active {
				select {
				case <-runtime.started:
				case <-ctx.Done():
					t.Fatal("an admitted build never reached the runtime")
				}
			}
			select {
			case <-runtime.started:
				t.Fatal("a capacity-waiting request reached the runtime")
			default:
			}
			if entitlement {
				var charged int
				if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_usage_attempts WHERE state IN ('reserved','running')`).Scan(&charged); err != nil {
					t.Fatal(err)
				}
				if charged != active {
					t.Fatalf("%d usage reservations for %d executing builds; waiting work must not reserve allowance", charged, active)
				}
			}
		})
	}
}
