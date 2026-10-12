package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/moby/moby/api/types/container"
)

// Output read after startup hides what the startup log hid: the labelled
// variables, passwords inside URLs, and, for a container from before the
// label, everything but Previa's public variables.
func TestOutputRedactorHidesTheLabelledSecrets(t *testing.T) {
	env := []string{"PORT=3000", "NODE_ENV=production", "API_TOKEN=token-value-123", "DATABASE_URL=postgres://previa:pw-secret-9876@postgres-x:5432/previa", "PUBLIC_URL=https://example.com/app"}
	labelled := container.InspectResponse{Config: &container.Config{Env: env, Labels: map[string]string{dockerx.LabelRedactEnv: "API_TOKEN,DATABASE_URL"}}}
	r, err := outputRedactor(labelled)
	if err != nil {
		t.Fatal(err)
	}
	out := string(r.Redact([]byte("token-value-123 pw-secret-9876 production https://example.com/app")))
	if strings.Contains(out, "token-value-123") || strings.Contains(out, "pw-secret-9876") || !strings.Contains(out, "production") || !strings.Contains(out, "https://example.com/app") {
		t.Fatalf("labelled redaction: %q", out)
	}
	legacy := container.InspectResponse{Config: &container.Config{Env: env}}
	if r, err = outputRedactor(legacy); err != nil {
		t.Fatal(err)
	}
	out = string(r.Redact([]byte("token-value-123 production https://example.com/app 3000")))
	if strings.Contains(out, "token-value-123") || strings.Contains(out, "production") || strings.Contains(out, "https://example.com/app") {
		t.Fatalf("a container without the label hides every non-public value: %q", out)
	}
}

func TestStopNoticeSaysWhatHappensNext(t *testing.T) {
	at := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		crash Crash
		state container.InspectResponse
		want  string
	}{
		{Crash{ExitCode: 1}, container.InspectResponse{State: &container.State{Restarting: true}, RestartCount: 0},
			"The app stopped at 12 Oct 2026, 09:30:00 UTC with exit code 1. Docker is restarting it (restart 1 of 3)."},
		{Crash{ExitCode: 137}, container.InspectResponse{State: &container.State{OOMKilled: true}, RestartCount: 3},
			"The app stopped at 12 Oct 2026, 09:30:00 UTC: it ran out of memory (exit code 137). It was restarted 3 times and is no longer restarted; build again after fixing it."},
		{Crash{ExitCode: 0}, container.InspectResponse{State: &container.State{}},
			"The app stopped at 12 Oct 2026, 09:30:00 UTC with exit code 0. It exited on its own, so it is not restarted; build again to bring it back."},
	} {
		if got := stopNotice("The app", tc.crash, tc.state, at); got != tc.want {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
}

// A crash is recorded once, only for the candidate the environment serves
// and never during a teardown.
func TestRecordCrashOnlyForTheLivePreviewOnce(t *testing.T) {
	pool := isolatedPool(t)
	ctx := t.Context()
	var owner, install, repo, pr, env int64
	scan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	scan(&owner, `INSERT INTO users(github_id,username) VALUES (42,'owner') RETURNING id`)
	scan(&install, `INSERT INTO installations(github_installation_id,owner_user_id) VALUES (99,$1) RETURNING id`, owner)
	scan(&repo, `INSERT INTO repos(installation_id,github_repo_id,full_name) VALUES ($1,100,'owner/app') RETURNING id`, install)
	scan(&pr, `INSERT INTO pull_requests(repo_id,number,head_sha,generation,head_repo_id) VALUES ($1,7,'sha-1',4,100) RETURNING id`, repo)
	scan(&env, `INSERT INTO environments(pull_request_id,subdomain,status,container_id,applied_generation,applied_revision,expires_at)
		VALUES ($1,'preview-7','running',$2,4,2,now()+interval '1 day') RETURNING id`, pr, strings.Repeat("a", 64))
	store := NewTargetStore(pool)
	crash := Crash{ContainerID: strings.Repeat("a", 64), EventNanos: 1, EnvironmentID: env, Generation: 4, Revision: 2, ExitCode: 1}
	for i, tc := range []struct {
		change func(*Crash)
		want   bool
	}{
		{func(*Crash) {}, true},
		{func(*Crash) {}, false},                                    // the same event, from another worker
		{func(c *Crash) { c.EventNanos = 2 }, true},                 // the next crash
		{func(c *Crash) { c.EventNanos, c.Revision = 3, 1 }, false}, // an older candidate
	} {
		tc.change(&crash)
		if got, err := store.RecordCrash(ctx, crash); err != nil || got != tc.want {
			t.Fatalf("case %d: %v %v, want %v", i, got, err, tc.want)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE pull_requests SET generation=5, teardown_requested_generation=5 WHERE id=$1", pr); err != nil {
		t.Fatal(err)
	}
	crash.EventNanos, crash.Revision = 4, 2
	if got, err := store.RecordCrash(ctx, crash); err != nil || got {
		t.Fatalf("a teardown's stop is no crash: %v %v", got, err)
	}
}
