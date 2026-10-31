package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/ajaykathwatee/previa/internal/activity"
	"github.com/ajaykathwatee/previa/internal/buildlogs"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/runtime"
	"github.com/ajaykathwatee/previa/internal/sampledata"
	"github.com/ajaykathwatee/previa/internal/secrets"
	"github.com/jackc/pgx/v5"
)

// Persistent test data (docs/32-persistent-test-data.md) connects the
// sampledata feature with activity, build logs, the runtime and the workers'
// capabilities through these adapters.

// testDataActivity records Test data events as repository activity for the
// people who may build in the repository.
type testDataActivity struct{ recorder *activity.Recorder }

func (a testDataActivity) RecordTx(ctx context.Context, tx pgx.Tx, e sampledata.Event) error {
	return a.recorder.RecordTx(ctx, tx, activity.Event{
		RepoID: e.RepoID, ActorID: e.ActorID, Kind: e.Kind, Visibility: activity.VisibilityOperators, Metadata: e.Metadata, DedupKey: e.DedupKey,
	})
}

// workerTestData tells the Test data tab what the execution workers offer:
// PostgreSQL or MySQL, and baseline storage.
type workerTestData struct{ capabilities *workers.Reader }

func (w workerTestData) TestData(ctx context.Context) (sampledata.Offer, error) {
	caps, err := w.capabilities.Current(ctx)
	if err != nil {
		return sampledata.Offer{}, fmt.Errorf("read worker capabilities: %w", err)
	}
	offer := sampledata.Offer{Baselines: caps.Baselines}
	for _, s := range caps.Services {
		offer.Databases = offer.Databases || strings.HasPrefix(s, "postgres ") || strings.HasPrefix(s, "mysql ")
	}
	return offer, nil
}

// baselineLogs reads refresh logs from the build log store.
type baselineLogs struct{ store *buildlogs.Store }

func (b baselineLogs) LinesBefore(ctx context.Context, refreshID, before int64, limit int, source, query string) ([]sampledata.LogLine, error) {
	filter, err := buildlogs.ParseFilter(source, query)
	if err != nil {
		return nil, err
	}
	lines, err := b.store.BaselineLinesBefore(ctx, refreshID, before, limit, filter)
	return logLines(lines), err
}

func (b baselineLogs) LinesFrom(ctx context.Context, refreshID, from int64, limit int) ([]sampledata.LogLine, error) {
	lines, err := b.store.BaselineLinesFrom(ctx, refreshID, from, limit)
	return logLines(lines), err
}

func logLines(lines []buildlogs.Line) []sampledata.LogLine {
	out := make([]sampledata.LogLine, len(lines))
	for i, l := range lines {
		out[i] = sampledata.LogLine{Sequence: l.Sequence, System: l.System, Text: l.Text, At: l.At}
	}
	return out
}

// startingData gives candidates their repository's Test data plan and
// baselines. Without baseline storage (no secret keys on the worker) only
// kept data applies.
type startingData struct {
	store   *sampledata.Store
	storage *sampledata.Storage // nil: baselines cannot be read here
}

func (s startingData) Plan(ctx context.Context, repoID int64) (runtime.DataPlan, error) {
	settings, baselines, err := s.store.Plan(ctx, repoID)
	if err != nil {
		return runtime.DataPlan{}, err
	}
	plan := runtime.DataPlan{Enabled: settings.Enabled, Keep: settings.KeepData, UseBaseline: settings.UseBaseline && s.storage != nil}
	if !plan.UseBaseline {
		return plan, nil
	}
	for _, b := range baselines {
		plan.Baselines = append(plan.Baselines, runtime.Baseline{
			ID: b.ID, Kind: b.Kind, Version: b.Version, Branch: b.Branch, CommitSHA: b.CommitSHA,
			StorageKey: b.StorageKey, Checksum: b.Checksum, KeyVersion: b.KeyVersion, Size: b.Size, BuiltAt: b.BuiltAt,
		})
	}
	return plan, nil
}

func (s startingData) OpenBaseline(_ context.Context, repoID int64, b runtime.Baseline) (io.ReadCloser, error) {
	if s.storage == nil {
		return nil, errors.New("baseline storage is not configured on this worker")
	}
	return s.storage.Open(repoID, b.StorageKey, b.Checksum)
}

func (s startingData) BaselineUnusable(ctx context.Context, repoID int64, b runtime.Baseline) error {
	return s.store.Unusable(ctx, repoID, b.ID)
}

// baselineBuilder builds a baseline with the runtime, its log in the build
// log store and its dumps in baseline storage.
type baselineBuilder struct {
	runtime *runtime.Runtime
	logs    *buildlogs.Store
	storage *sampledata.Storage
	logger  *slog.Logger
}

func (b baselineBuilder) BuildBaseline(ctx context.Context, job sampledata.BuildJob) (sampledata.BuildResult, error) {
	var stored []sampledata.Stored
	result, err := b.runtime.RunBaseline(ctx, runtime.BaselineJob{
		RepoID: job.RepoID, GitHubRepoID: job.GitHubRepoID, InstallationID: job.InstallationID, WorkspaceID: job.WorkspaceID,
		RefreshID: job.RefreshID, Branch: job.Branch, CommitSHA: job.CommitSHA,
		OpenLogs: func(ctx context.Context, redactor *secrets.Redactor) (runtime.LogSink, error) {
			return b.logs.OpenBaseline(ctx, job.RefreshID, redactor)
		},
		Capture: func(string, string) (runtime.DumpSink, error) {
			w, err := b.storage.Create(job.RepoID)
			if err != nil {
				return nil, err
			}
			return &dumpSink{w: w, stored: &stored}, nil
		},
	})
	if err != nil {
		// Dumps stored before the failure are not recorded anywhere.
		for _, s := range stored {
			if removeErr := b.storage.Remove(s.Key); removeErr != nil {
				b.logger.WarnContext(ctx, "remove an unused baseline file", "refresh_id", job.RefreshID, "error", removeErr)
			}
		}
		var failed *runtime.BaselineError
		if errors.As(err, &failed) {
			return sampledata.BuildResult{}, &sampledata.BuildError{Code: failed.Code, Detail: failed.Detail, Retryable: failed.Retryable, Err: err}
		}
		return sampledata.BuildResult{}, err
	}
	out := sampledata.BuildResult{ConfigHash: result.ConfigHash}
	for _, d := range result.Dumps {
		out.Dumps = append(out.Dumps, sampledata.Dump{Kind: d.Kind, Version: d.Version,
			Stored: sampledata.Stored{Key: d.StorageKey, Checksum: d.Checksum, Size: d.Size, KeyVersion: d.KeyVersion}})
	}
	return out, nil
}

// dumpSink stores one captured dump, reporting the size limit the way the
// runtime expects.
type dumpSink struct {
	w      *sampledata.Writer
	stored *[]sampledata.Stored
}

func (d *dumpSink) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if errors.Is(err, sampledata.ErrTooLarge) {
		return n, runtime.ErrCaptureTooLarge
	}
	return n, err
}

func (d *dumpSink) Commit() (runtime.CapturedDump, error) {
	s, err := d.w.Commit()
	if errors.Is(err, sampledata.ErrTooLarge) {
		return runtime.CapturedDump{}, runtime.ErrCaptureTooLarge
	}
	if err != nil {
		return runtime.CapturedDump{}, err
	}
	*d.stored = append(*d.stored, s)
	return runtime.CapturedDump{StorageKey: s.Key, Checksum: s.Checksum, Size: s.Size, KeyVersion: s.KeyVersion}, nil
}

func (d *dumpSink) Abort() { d.w.Abort() }
