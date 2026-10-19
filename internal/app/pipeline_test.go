package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/onboarding"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/google/go-github/v91/github"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

type noApp struct{}

func (noApp) InstallationClient(int64) (*github.Client, error) { return nil, errors.New("unused") }
func (noApp) App(context.Context) (*github.App, error)         { return nil, errors.New("unused") }

func registered(t *testing.T, p *pipeline) *river.Config {
	t.Helper()
	rc := &river.Config{Workers: river.NewWorkers(), Queues: map[string]river.QueueConfig{}}
	for _, register := range p.register {
		register(rc)
	}
	return rc
}

func TestPipelineRegistersOnlyWhatItsDependenciesAllow(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	base := config.WorkerConfig{MaxConcurrentBuilds: 2}
	// The pool connects lazily; registering workers never reaches it.
	pool, err := pgxpool.New(t.Context(), "postgres://previa@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Without the GitHub App: local erasure, log and activity retention still run.
	p, err := buildPipeline(t.Context(), pipelineInput{cfg: base, pool: pool, logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	rc := registered(t, p)
	if _, activity := rc.Queues["activity_prune"]; p.capabilities.Execution || p.capabilities.Discovery || len(rc.Queues) != 3 || !activity || !strings.Contains(strings.Join(p.unavailable, " "), "GITHUB_APP_ID") {
		t.Fatalf("no App: %+v %v %v", p.capabilities, rc.Queues, p.unavailable)
	}

	// With the App but no preview domain: discovery and setup checks only; nothing builds.
	p, err = buildPipeline(t.Context(), pipelineInput{cfg: base, pool: pool, github: noApp{}, logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	rc = registered(t, p)
	if p.capabilities.Execution || !p.capabilities.Discovery || !strings.Contains(strings.Join(p.unavailable, " "), "PREVIEW_BASE_DOMAIN") {
		t.Fatalf("no routing: %+v %v", p.capabilities, p.unavailable)
	}
	for _, queue := range []string{"build", "environment_coordination", "environment_route", "housekeeping"} {
		if _, ok := rc.Queues[queue]; ok {
			t.Fatalf("queue %s registered without execution", queue)
		}
	}
	if _, ok := rc.Queues[onboarding.CheckQueue]; !ok {
		t.Fatal("setup checks need only the App and GitHub, not a preview domain")
	}

	// A preview domain without Docker is a configuration error, not a quiet
	// worker that never builds.
	routed := base
	routed.PreviewBaseDomain = "preview.example.com"
	if _, err := buildPipeline(t.Context(), pipelineInput{cfg: routed, pool: pool, github: noApp{}, logger: logger}); err == nil || !strings.Contains(err.Error(), "Docker") {
		t.Fatalf("routing without Docker: %v", err)
	}
}

// Registration requires neither a GitHub request nor Docker, even while paused.
func (noApp) Installation(context.Context, int64) (*github.Installation, error) {
	return nil, errors.New("unused")
}
func TestInstallationReconciliationRegisteredWithoutExecution(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://previa@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := config.WorkerConfig{}
	cfg.GitHubAppID = 77
	cfg.BuildsPaused = true
	p, err := buildPipeline(t.Context(), pipelineInput{cfg: cfg, pool: pool, github: noApp{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	rc := registered(t, p)
	if queue, ok := rc.Queues[installations.ReconciliationQueue]; !ok || queue.MaxWorkers != 1 || p.capabilities.Execution {
		t.Fatal("reconciliation depends on physical execution")
	}
	if len(rc.PeriodicJobs) == 0 {
		t.Fatal("reconciliation has no periodic wakeup")
	}
}
