package auth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/auth/dbgen"
	"github.com/alexedwards/scs/pgxstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Requires make migrate-up and TEST_DATABASE_URL pointing to the development DB.
func TestPostgresLoginPersistenceAndIdentityRename(t *testing.T) {
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set TEST_DATABASE_URL to verify PostgreSQL user/session persistence")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback auth test: %v", err)
		}
	}()
	users := NewRepository(tx)
	provider := &fakeProvider{identity: Identity{ID: time.Now().UnixNano(), Username: "auth-test", AvatarURL: "https://example.com/first"}}
	store := pgxstore.NewWithCleanupInterval(pool, 0)
	b, _, sessions := newTestBrowser(t, store, users, provider)
	state := b.loginState()
	anonymousCookie := b.cookies["previa_session"].Value
	defer func() {
		if err := store.DeleteCtx(context.Background(), storeToken(anonymousCookie)); err != nil {
			t.Errorf("cleanup anonymous session: %v", err)
		}
	}()
	if w := b.request("GET", "/auth/github/callback?code=valid&state="+state, ""); w.Code != 303 {
		t.Fatalf("PostgreSQL callback failed: %d %s", w.Code, w.Body)
	}
	token := storeToken(b.cookies["previa_session"].Value)
	defer func() {
		if err := store.DeleteCtx(context.Background(), token); err != nil {
			t.Errorf("cleanup authenticated session: %v", err)
		}
	}()
	queries := dbgen.New(pool)
	row, err := queries.GetSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Expiry.Time.After(time.Now()) || row.Expiry.Time.After(time.Now().Add(31*time.Minute)) {
		t.Fatal("persisted session expiry does not enforce idle timeout")
	}
	if _, err := queries.GetSession(ctx, storeToken(anonymousCookie)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("rotated token remains in PostgreSQL: %v", err)
	}
	_, values, err := sessions.Codec.Decode(row.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || values["user_id"] == nil || values["csrf_token"] == nil || values["session_epoch"] == nil {
		t.Fatalf("session should hold user ID, epoch and CSRF token; found keys: %v", values)
	}
	id := values["user_id"].(int64)
	before, err := users.GetUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := users.UpsertUser(ctx, Identity{ID: before.GitHubID, Username: "renamed-auth-test", AvatarURL: "https://example.com/second"})
	if err != nil || renamed.ID != before.ID || renamed.Username != "renamed-auth-test" || renamed.AvatarURL != "https://example.com/second" {
		t.Fatalf("rename changed identity or lost profile: %+v %v", renamed, err)
	}
	// A fresh session manager simulates a server restart with the same DB store.
	restarted, _, _ := newTestBrowser(t, store, users, provider)
	restarted.cookies = b.cookies
	if w := restarted.request("GET", "/protected", ""); w.Code != 200 {
		t.Fatalf("persisted session failed after manager restart: %d", w.Code)
	}
	csrf := values["csrf_token"].(string)
	if w := restarted.request("POST", "/auth/logout", "csrf_token="+csrf); w.Code != 303 {
		t.Fatalf("PostgreSQL logout failed: %d", w.Code)
	}
	if _, err := queries.GetSession(ctx, token); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("logout did not delete PostgreSQL row: %v", err)
	}
}
