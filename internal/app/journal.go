package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/ajaykathwatee/previa/internal/accounts"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/ajaykathwatee/previa/internal/sampledata"
	"github.com/ajaykathwatee/previa/internal/workspaces"
)

// JournalCommand is a private deployment command, never an HTTP route. The
// caller must encrypt/upload exports before invoking journal-ack, and must stop
// every serving/execution process before journal-replay.
func JournalCommand(ctx context.Context, name string, input io.Reader, output io.Writer) error {
	cfg, err := config.LoadServer()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := openDatabase(ctx, cfg.SharedConfig, "journal", cfg.AppEnv, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	service := accounts.NewService(pool)
	switch name {
	case "journal-snapshot":
		if _, err = pool.Exec(ctx, `UPDATE deletion_journal_metadata SET snapshot_at=clock_timestamp() WHERE singleton`); err != nil {
			return fmt.Errorf("record backup boundary: %w", err)
		}
		return nil
	case "journal-export":
		b, err := service.ExportJournal(ctx, cfg.DeletionJournalDays, cfg.DeletionJournalMaxBytes)
		if err != nil {
			return err
		}
		if _, err = output.Write(b); err != nil {
			return fmt.Errorf("write journal export: %w", err)
		}
		return nil
	case "journal-ack", "journal-replay":
		b, err := io.ReadAll(io.LimitReader(input, int64(cfg.DeletionJournalMaxBytes)+1))
		if err != nil {
			return fmt.Errorf("read deletion journal: %w", err)
		}
		journal, err := accounts.DecodeJournal(b, cfg.DeletionJournalMaxBytes)
		if err != nil {
			return err
		}
		if name == "journal-ack" {
			return service.AcknowledgeJournal(ctx, journal)
		}
		producer, err := queue.NewProducer(pool, logger)
		if err != nil {
			return err
		}
		operations, err := environments.NewOperationBroker(producer)
		if err != nil {
			return err
		}
		repo := environments.NewRepository(pool, operations)
		data, err := sampledata.NewStore(pool, producer, environments.BuildQueue)
		if err != nil {
			return err
		}
		service.WithPreviews(repositoryLifecycle{environments.NewLifecycle(repo, environments.NewRequestBroker(pool, producer), nil), sampledata.NewLifecycle(data, nil, nil)})
		service.WithAccountFences(workspaces.NewService(workspaces.NewRepository(pool), nil, nil, nil))
		return service.ReplayJournal(ctx, journal)
	}
	return fmt.Errorf("unknown journal command %q", name)
}
