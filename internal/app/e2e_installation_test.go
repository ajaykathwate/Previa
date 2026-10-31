package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/google/go-github/v91/github"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"
)

// The TLS GitHub fixture has no old installation endpoint after uninstall; its
// exact App GET returns 404. No signed installation delivery is sent in this phase.
type missedInstallationApp struct{ *fakeGitHub }

func (a missedInstallationApp) Installation(ctx context.Context, id int64) (*github.Installation, error) {
	c, err := a.InstallationClient(id)
	if err != nil {
		return nil, err
	}
	installation, _, err := c.Apps.GetInstallation(ctx, id)
	return installation, err
}
func proveMissedUninstall(t *testing.T, ctx context.Context, pool *pgxpool.Pool, api *fakeGitHub, broker *environments.Broker, docker *client.Client, router *e2eRouter, instance string, user, repo, pr int64, host, marker string) {
	t.Helper()
	policy, err := installations.NewBuildPolicyService(installations.NewRepository(pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.Set(ctx, user, repo, installations.ManualBuilds); err != nil {
		t.Fatal(err)
	}
	// A second healthy known installation keeps the mass-revocation breaker armed.
	if _, err = pool.Exec(ctx, `INSERT INTO installations(github_installation_id) VALUES(902)`); err != nil {
		t.Fatal(err)
	}
	delivery := webhooks.NewDeliveryService(pool, broker, nil)
	reconciler := installations.NewReconciler(installations.NewRepository(pool), missedInstallationApp{api}, delivery, slog.New(slog.NewTextHandler(io.Discard, nil)), e2eAppID, 5)
	if err = reconciler.Run(ctx, 900); err != nil {
		t.Fatal(err)
	}
	e2eWait(t, ctx, "missed uninstall cleanup without GitHub access", 3*time.Minute, func() (string, error) {
		var state string
		if err := pool.QueryRow(ctx, `SELECT status FROM environments WHERE pull_request_id=$1`, pr).Scan(&state); err != nil {
			return "", err
		}
		if state != "torn_down" {
			return "", fmt.Errorf("environment %s", state)
		}
		return "", e2eLeftovers(ctx, docker, instance, pr)
	})
	if body, err := router.get(host); err == nil && body == marker {
		t.Fatal("missed uninstall left the preview serving")
	}
	before := e2eCount(t, pool, `SELECT count(*) FROM workspace_usage_attempts`)
	if err = installations.NewRepository(pool).Link(ctx, 901, user, 0, e2eOwner, []installations.RepositoryInfo{{ID: e2eRepoID, FullName: e2eOwner + "/" + e2eRepo}}); err != nil {
		t.Fatal("same-workspace reinstall failed", err)
	}
	if e2eCount(t, pool, `SELECT count(*) FROM repos WHERE id=$1 AND installation_id=(SELECT id FROM installations WHERE github_installation_id=901)`, repo) != 1 {
		t.Fatal("reinstall lost repository identity")
	}
	if e2eCount(t, pool, `SELECT count(*) FROM build_intents WHERE pull_request_id=$1 AND state IN ('pending','queued','running')`, pr) != 0 || e2eCount(t, pool, `SELECT count(*) FROM workspace_usage_attempts`) != before {
		t.Fatal("restoring access authorized a build")
	}
	if err = delivery.Process(ctx, "late-uninstall", "installation", "late", &github.InstallationEvent{Action: new("deleted"), Installation: &github.Installation{ID: new(int64(900)), UpdatedAt: &github.Timestamp{Time: time.Now()}}}); err != nil {
		t.Fatal(err)
	}
	t.Log("missed uninstall: dropped webhook, exact App 404, real teardown complete; same-workspace reinstall preserves identity and starts no build")
}
