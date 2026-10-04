package realtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDecodeIgnoresForeignPayloads(t *testing.T) {
	if topic, source, err := decode(`{"topic":"repository:7","source":"pr_syncs"}`); err != nil || topic != "repository:7" || source != "pr_syncs" {
		t.Fatalf("decode = %q %q %v", topic, source, err)
	}
	for _, bad := range []string{"", "plain", `{"source":"x"}`} {
		if _, _, err := decode(bad); err == nil {
			t.Errorf("decode(%q) accepted", bad)
		}
	}
}

// Committed writes in an isolated, migrated schema reach a hub subscriber
// through the triggers and Listen; rolled-back writes stay silent.
func TestTriggersNotifyListenerIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for isolated database tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("realtime_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		defer admin.Close()
		if _, err := admin.Exec(c, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	files, err := filepath.Glob("../../../db/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v", err)
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}

	hub := NewHub()
	listening := make(chan struct{})
	// The resync published after LISTEN tells us the session is ready.
	ready := hub.Subscribe("ready", Cursor{})
	go func() { <-ready.Ready(); close(listening) }()
	listenCtx, stop := context.WithCancel(ctx)
	defer stop()
	go Listen(listenCtx, func(c context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(c, pool.Config().ConnConfig.Copy())
	}, hub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	select {
	case <-listening:
	case <-ctx.Done():
		t.Fatal("listener did not start")
	}

	var userID, installationID, repoID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users(github_id,username) VALUES (42,'owner') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES (99,$1) RETURNING id`, userID).Scan(&installationID); err != nil {
		t.Fatal(err)
	}
	// Identity values restart in every schema; a unique ID keeps topics distinct.
	if err := pool.QueryRow(ctx, `INSERT INTO repos(id,installation_id,full_name) OVERRIDING SYSTEM VALUE VALUES ($1,$2,$3) RETURNING id`, 1_000_000_000+time.Now().UnixNano()%1_000_000_000, installationID, schema+"/repo").Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	topic := fmt.Sprintf("repository:%d", repoID)
	// Other test schemas share the channel and may reuse IDs, so wait for a
	// matching source rather than asserting that nothing else arrives.
	await := func(source string, write func() error) {
		t.Helper()
		sub := hub.Subscribe(topic, Cursor{})
		defer sub.Close()
		if err := write(); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(10 * time.Second)
		for {
			select {
			case <-sub.Ready():
				if e, ok := sub.Next(); ok && e.Kind == KindChange && e.Source == source {
					return
				}
			case <-deadline:
				t.Fatalf("no %s notification for %s", source, topic)
			}
		}
	}
	await("pull_requests", func() error {
		_, err := pool.Exec(ctx, `INSERT INTO pull_requests(repo_id,number,head_sha) VALUES ($1,1,'sha')`, repoID)
		return err
	})
	await("pr_syncs", func() error {
		_, err := pool.Exec(ctx, `INSERT INTO pr_syncs(repo_id,state) VALUES ($1,'queued')`, repoID)
		return err
	})
	await("repos", func() error {
		_, err := pool.Exec(ctx, `UPDATE repos SET build_mode='automatic' WHERE id=$1`, repoID)
		return err
	})
	await("installations", func() error {
		_, err := pool.Exec(ctx, `UPDATE installations SET status='suspended' WHERE id=$1`, installationID)
		return err
	})

	// No-op updates and rolled-back writes must not notify.
	sub := hub.Subscribe(topic, Cursor{})
	defer sub.Close()
	if _, err := pool.Exec(ctx, `UPDATE pull_requests SET head_sha=head_sha WHERE repo_id=$1`, repoID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE pull_requests SET head_sha='rolled-back' WHERE repo_id=$1`, repoID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// A committed sentinel proves the listener has drained everything before it.
	sentinel := hub.Subscribe("sentinel", Cursor{})
	defer sentinel.Close()
	if _, err := pool.Exec(ctx, `SELECT pg_notify('previa_events', '{"topic":"sentinel"}')`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sentinel.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("sentinel not delivered")
	}
	for {
		e, ok := sub.Next()
		if !ok {
			break
		}
		if e.Source == "pull_requests" {
			t.Fatalf("no-op or rolled-back write notified: %+v", e)
		}
	}
}
