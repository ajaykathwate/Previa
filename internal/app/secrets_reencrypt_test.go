package app

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/reviews"
	"github.com/ajaykathwatee/previa/internal/secrets"
)

// Test access is sealed with the repository secrets' keys, so key rotation
// re-encrypts it in the same run.
func TestReencryptionCoversTestAccess(t *testing.T) {
	pool := e2eSchema(t)
	ctx := t.Context()
	key := func(version string) string {
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		return version + ":" + base64.StdEncoding.EncodeToString(raw)
	}
	v1 := key("1")
	old, err := secrets.ParseKeyring(v1, "1")
	if err != nil {
		t.Fatal(err)
	}
	current, err := secrets.ParseKeyring(v1+","+key("2"), "2")
	if err != nil {
		t.Fatal(err)
	}
	var user, repoID int64
	e2eScan(t, pool, "INSERT INTO users(github_id, username) VALUES (1, 'owner') RETURNING id", &user)
	e2eScan(t, pool, `WITH i AS (INSERT INTO installations(github_installation_id, owner_user_id) VALUES (900, $1) RETURNING id)
		INSERT INTO repos(installation_id, full_name, github_repo_id) SELECT id, 'octo/app', 100 FROM i RETURNING id`, &repoID, user)
	sealed, version, err := old.Seal(repoID, reviews.TestAccessName, []byte("preview-password"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO review_test_access(repo_id, account, secret_ciphertext, key_version, updated_by) VALUES ($1, 'qa@preview.test', $2, $3, $4)",
		repoID, sealed, version, user); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := reencryptPass(ctx, &out, "Test access", testAccessRotation{store: reviews.NewRotation(pool)}, current); err != nil {
		t.Fatal(err)
	}
	want := "Test access: re-encrypted 1, skipped 0 (changed during the run), active key version 2\n" +
		"Test access: every value uses the active key; older key versions can be removed\n"
	if out.String() != want {
		t.Fatalf("report:\n%s", out.String())
	}
	if strings.Contains(out.String(), "preview-password") || strings.Contains(out.String(), "qa@preview.test") {
		t.Fatal("the report names a value")
	}
	e2eScan(t, pool, "SELECT secret_ciphertext FROM review_test_access WHERE repo_id=$1 AND key_version=2", &sealed, repoID)
	if plain, err := current.Open(repoID, reviews.TestAccessName, 2, sealed); err != nil || string(plain) != "preview-password" {
		t.Fatalf("after rotation: %v", err)
	}
}
