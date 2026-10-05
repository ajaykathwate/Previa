package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/google/go-github/v91/github"
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

	// Without the GitHub App: log retention only, and the reason is stated.
	p, err := buildPipeline(t.Context(), pipelineInput{cfg: base, logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	rc := registered(t, p)
	if p.capabilities.Execution || p.capabilities.Discovery || len(rc.Queues) != 1 || !strings.Contains(strings.Join(p.unavailable, " "), "GITHUB_APP_ID") {
		t.Fatalf("no App: %+v %v %v", p.capabilities, rc.Queues, p.unavailable)
	}

	// With the App but no preview domain: discovery only; nothing builds.
	p, err = buildPipeline(t.Context(), pipelineInput{cfg: base, github: noApp{}, logger: logger})
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

	// A preview domain without Docker is a configuration error, not a quiet
	// worker that never builds.
	routed := base
	routed.PreviewBaseDomain = "preview.example.com"
	if _, err := buildPipeline(t.Context(), pipelineInput{cfg: routed, github: noApp{}, logger: logger}); err == nil || !strings.Contains(err.Error(), "Docker") {
		t.Fatalf("routing without Docker: %v", err)
	}
}
