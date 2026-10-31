package reviews

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/activity"
	"github.com/ajaykathwatee/previa/internal/secrets"
)

// testKey is one random SECRETS_ENCRYPTION_KEYS entry.
func testKey(version string) string {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return version + ":" + base64.StdEncoding.EncodeToString(key)
}

func keyring(t *testing.T, spec, active string) *secrets.Keyring {
	t.Helper()
	keys, err := secrets.ParseKeyring(spec, active)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// sealing is the fixture's service with test-access encryption.
func (f *fixture) sealing(t *testing.T, sealer Sealer) *Service {
	t.Helper()
	service, err := NewService(f.pool, timeline{recorder: activity.NewRecorder()}, Options{PreviewDomain: "preview.test", Sealer: sealer})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestTestAccessIsSealedAndRevealedOnRequest(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	keys := keyring(t, testKey("1"), "1")
	service := f.sealing(t, keys)
	const password = "  correct horse 7  " // surrounding spaces are part of it

	p, err := service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	if err != nil || p.TestAccess.Set() || p.TestAccess.CanEdit || !p.TestAccess.Sealing {
		t.Fatalf("before any test access: %v %+v", err, p.TestAccess)
	}
	if _, err := service.RevealTestAccess(ctx, f.as(f.reviewer)); !errors.Is(err, ErrNoTestAccess) {
		t.Fatalf("nothing to reveal: %v", err)
	}
	in := TestAccessInput{Account: "reviewer@preview.test", Secret: password}
	for name, user := range map[string]int64{"developer": f.dev, "reviewer": f.reviewer} {
		if err := service.SaveTestAccess(ctx, f.as(user), in); !errors.Is(err, ErrUnavailable) {
			t.Errorf("a %s cannot set test access: %v", name, err)
		}
	}
	if err := service.SaveTestAccess(ctx, f.as(f.admin), in); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	var version int32
	if err := f.pool.QueryRow(ctx, "SELECT secret_ciphertext, key_version FROM review_test_access WHERE repo_id=$1", f.repo).Scan(&sealed, &version); err != nil {
		t.Fatal(err)
	}
	if version != 1 || bytes.Contains(sealed, []byte(strings.TrimSpace(password))) {
		t.Fatalf("stored in the clear or with the wrong key: version %d", version)
	}
	p, _ = service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	if a := p.TestAccess; a.Account != "reviewer@preview.test" || !a.HasSecret || a.Revision != 1 || a.UpdatedBy.ID != f.admin || a.Revealed != "" || a.CanEdit {
		t.Fatalf("a Reviewer's view: %+v", a)
	}
	if p, _ = service.Page(ctx, f.owner, f.repo, 42, Query{}); !p.TestAccess.CanEdit {
		t.Fatal("Owners edit test access")
	}
	// Someone else saved meanwhile; a repeated save changes nothing.
	var input *InputError
	if err := service.SaveTestAccess(ctx, f.as(f.owner), TestAccessInput{Account: "other@preview.test"}); !errors.As(err, &input) || !strings.Contains(input.Message, "changed the test access meanwhile") {
		t.Fatalf("a save from an older form: %v", err)
	}
	if err := service.SaveTestAccess(ctx, f.as(f.owner), TestAccessInput{Account: "reviewer@preview.test", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if p, _ = service.Page(ctx, f.owner, f.repo, 42, Query{}); p.TestAccess.Revision != 1 {
		t.Fatalf("a repeated save made revision %d", p.TestAccess.Revision)
	}

	// Members with preview access reveal it; each reveal is recorded.
	value, err := service.RevealTestAccess(ctx, f.as(f.reviewer))
	if err != nil || value != password {
		t.Fatalf("reveal: %v (matches: %t)", err, value == password)
	}
	p, _ = service.Page(ctx, f.owner, f.repo, 42, Query{})
	if p.TestAccess.LastRevealedBy.ID != f.reviewer || p.TestAccess.LastRevealedAt == "" {
		t.Fatalf("the reveal is not shown: %+v", p.TestAccess)
	}
	for name, attempt := range map[string]Actor{"outsider": {UserID: f.outsider, RepoID: f.repo, Number: 42}, "developer without a grant": {UserID: f.newdev, RepoID: f.repo, Number: 42}} {
		if _, err := service.RevealTestAccess(ctx, attempt); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Sealed values are bound to their repository: copied to another one,
	// they do not open.
	if _, err := f.pool.Exec(ctx, `INSERT INTO review_test_access(repo_id, account, secret_ciphertext, key_version, updated_by)
		SELECT $2, account, secret_ciphertext, key_version, updated_by FROM review_test_access WHERE repo_id=$1`, f.repo, f.otherRepo); err != nil {
		t.Fatal(err)
	}
	if value, err := service.RevealTestAccess(ctx, Actor{UserID: f.owner, RepoID: f.otherRepo, Number: 7}); err == nil || value != "" {
		t.Fatal("a value copied to another repository opened")
	}
	// Nor does a repository secret: reveal opens test access only.
	runtimeSecret, version, err := keys.Seal(f.otherRepo, "TEST_LOGIN_PASSWORD", []byte("runtime-only"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE review_test_access SET secret_ciphertext=$2, key_version=$3 WHERE repo_id=$1", f.otherRepo, runtimeSecret, version); err != nil {
		t.Fatal(err)
	}
	if value, err := service.RevealTestAccess(ctx, Actor{UserID: f.owner, RepoID: f.otherRepo, Number: 7}); err == nil || value != "" {
		t.Fatal("a repository secret opened as test access")
	}

	// Removing the password keeps the account; clearing both removes it.
	if err := service.SaveTestAccess(ctx, f.as(f.owner), TestAccessInput{Account: "reviewer@preview.test", RemoveSecret: true, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if p, _ = service.Page(ctx, f.reviewer, f.repo, 42, Query{}); p.TestAccess.HasSecret || p.TestAccess.Account != "reviewer@preview.test" {
		t.Fatalf("after removing the password: %+v", p.TestAccess)
	}
	if _, err := service.RevealTestAccess(ctx, f.as(f.reviewer)); !errors.Is(err, ErrNoTestAccess) {
		t.Fatalf("a removed password is not revealed: %v", err)
	}
	if err := service.SaveTestAccess(ctx, f.as(f.owner), TestAccessInput{Revision: 2}); err != nil {
		t.Fatal(err)
	}
	if p, _ = service.Page(ctx, f.reviewer, f.repo, 42, Query{}); p.TestAccess.Set() {
		t.Fatalf("cleared test access: %+v", p.TestAccess)
	}
	want := []string{KindTestAccessUpdated, KindTestAccessRevealed, KindTestAccessUpdated, KindTestAccessUpdated}
	if got := f.events(t, f.pr); !slices.Equal(got, want) {
		t.Fatalf("timeline %v, want %v", got, want)
	}
	var leaked int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM activity_events WHERE strpos(metadata::text, $1) > 0", strings.TrimSpace(password)).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("the password reached the timeline: %v %d", err, leaked)
	}

	for name, bad := range map[string]TestAccessInput{
		"a two-line account": {Account: "one\ntwo"},
		"a line break":       {Secret: "pass\nword"},
		"a control":          {Secret: "pass\x1bword"},
		"too long":           {Secret: strings.Repeat("p", maxSecretBytes+1)},
		"invalid text":       {Secret: "\xff\xfe"},
	} {
		if err := service.SaveTestAccess(ctx, f.as(f.owner), bad); !errors.As(err, &input) || len(input.Fields) == 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTestAccessWithoutEncryptionKeepsTheAccountOnly(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var input *InputError
	if err := f.service.SaveTestAccess(ctx, f.as(f.owner), TestAccessInput{Account: "qa@preview.test", Secret: "hunter2"}); !errors.As(err, &input) || input.Fields["secret"] == "" {
		t.Fatalf("a password without encryption: %v", err)
	}
	if err := f.service.SaveTestAccess(ctx, f.as(f.owner), TestAccessInput{Account: "qa@preview.test"}); err != nil {
		t.Fatal(err)
	}
	p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	if p.TestAccess.Account != "qa@preview.test" || p.TestAccess.HasSecret || p.TestAccess.Sealing {
		t.Fatalf("account-only test access: %+v", p.TestAccess)
	}
	if _, err := f.service.RevealTestAccess(ctx, f.as(f.reviewer)); !errors.Is(err, ErrNoTestAccess) {
		t.Fatalf("nothing to reveal: %v", err)
	}
}

func TestTestAccessFollowsKeyRotation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	v1 := testKey("1")
	old := keyring(t, v1, "1")
	if err := f.sealing(t, old).SaveTestAccess(ctx, f.as(f.owner), TestAccessInput{Account: "qa@preview.test", Secret: "rotate-me"}); err != nil {
		t.Fatal(err)
	}
	// The new keyring keeps version 1 and makes version 2 active.
	current := keyring(t, v1+","+testKey("2"), "2")
	rotation := NewRotation(f.pool)
	due, err := rotation.NeedingRotation(ctx, 2, 0, 10)
	if err != nil || len(due) != 1 || due[0].RepoID != f.repo || due[0].Version != 1 {
		t.Fatalf("due for rotation: %v %+v", err, due)
	}
	plain, err := current.Open(f.repo, TestAccessName, due[0].Version, due[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	sealed, version, err := current.Seal(f.repo, TestAccessName, plain)
	if err != nil || version != 2 {
		t.Fatalf("reseal: %v version %d", err, version)
	}
	if ok, err := rotation.Rotate(ctx, due[0], sealed, version); err != nil || !ok {
		t.Fatalf("rotate: %v %t", err, ok)
	}
	if ok, err := rotation.Rotate(ctx, due[0], sealed, version); err != nil || ok {
		t.Fatalf("a value changed since it was read is not rotated again: %v %t", err, ok)
	}
	if counts, err := rotation.CountByVersion(ctx); err != nil || counts[2] != 1 || counts[1] != 0 {
		t.Fatalf("counts %v %v", counts, err)
	}
	if value, err := f.sealing(t, current).RevealTestAccess(ctx, f.as(f.reviewer)); err != nil || value != "rotate-me" {
		t.Fatalf("reveal after rotation: %v", err)
	}
	if _, err := f.sealing(t, old).RevealTestAccess(ctx, f.as(f.reviewer)); err == nil {
		t.Fatal("a keyring without the active key opened the rotated value")
	}
}

// Saving test access wakes the repository's live pages; a reveal does not,
// so the page of the person who revealed it is not refreshed away.
func TestTestAccessSavesWakeLivePagesButRevealsDoNot(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	service := f.sealing(t, keyring(t, testKey("1"), "1"))
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN previa_events"); err != nil {
		t.Fatal(err)
	}
	// The channel is shared with every package's tests running in parallel
	// (each in its own schema), so only this feature's tables count.
	next := func() string {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			n, err := conn.Conn().WaitForNotification(wait)
			if err != nil {
				t.Fatal("no change signal:", err)
			}
			if strings.Contains(n.Payload, `"repository:`) && (strings.Contains(n.Payload, "review_test_access") || strings.Contains(n.Payload, "preview_feedback")) {
				return n.Payload
			}
		}
	}
	if err := service.SaveTestAccess(ctx, f.as(f.owner), TestAccessInput{Account: "qa@preview.test", Secret: "hunter2-preview"}); err != nil {
		t.Fatal(err)
	}
	if payload := next(); !strings.Contains(payload, "review_test_access") || strings.Contains(payload, "hunter2") || strings.Contains(payload, "qa@preview") {
		t.Fatalf("save signal %s", payload)
	}
	if _, err := service.RevealTestAccess(ctx, f.as(f.reviewer)); err != nil {
		t.Fatal(err)
	}
	// Signals arrive in commit order: the next one must be this finding's.
	if _, err := service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Fence", RequestKey: key(1)}); err != nil {
		t.Fatal(err)
	}
	if payload := next(); !strings.Contains(payload, "preview_feedback") {
		t.Fatalf("a reveal woke live pages: %s", payload)
	}
}
