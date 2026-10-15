package environments_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/webhooks"
)

// secondAccount adds another account to the fixture's server: its own user,
// installation and Automatic repository.
func (f *fixture) secondAccount(t *testing.T) (repoID, ownerID int64) {
	t.Helper()
	var installID int64
	for _, step := range []struct {
		sql  string
		args []any
		into *int64
	}{
		{"INSERT INTO users(github_id,username) VALUES(2,'second') RETURNING id", nil, &ownerID},
		{"INSERT INTO installations(github_installation_id,owner_user_id) VALUES(2,$1) RETURNING id", []any{&ownerID}, &installID},
		{"INSERT INTO repos(installation_id,full_name,github_repo_id,build_mode) VALUES($1,'second/repo',4343,'automatic') RETURNING id", []any{&installID}, &repoID},
	} {
		args := make([]any, len(step.args))
		for i, a := range step.args {
			args[i] = *(a.(*int64))
		}
		if err := f.pool.QueryRow(t.Context(), step.sql, args...).Scan(step.into); err != nil {
			t.Fatal(err)
		}
	}
	return repoID, ownerID
}

// prEventIn opens PR number in repoID with an eligible Automatic request.
func (f *fixture) prEventIn(t *testing.T, repoID int64, number int32, sha string) env.Request {
	t.Helper()
	r, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{MergeableState: "clean", RepoID: repoID, Number: number, HeadSHA: sha, Action: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE build_intents SET available_at=now()-interval '1 second' WHERE pull_request_id=$1", r.PullRequestID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "INSERT INTO environments(pull_request_id,subdomain) VALUES($1,$2)", r.PullRequestID, fmt.Sprintf("preview-%d-%d", repoID, number)); err != nil {
		t.Fatal(err)
	}
	return r
}

// One server, two accounts: each keeps its own limit, waiting builds take
// turns by account, a daily budget waits until midnight UTC, and a finished
// build counts its time and wakes the next request in line.
func TestAccountsTakeTurnsAndKeepTheirLimits(t *testing.T) {
	f := newFixture(t)
	other, otherOwner := f.secondAccount(t)
	r := newGatedRuntime()
	s := newCapacityService(t, f, r)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// waits requires a wait for reason and returns until when, if it says.
	waits := func(request env.Request, reason string) time.Time {
		t.Helper()
		var waiting *env.WaitingError
		if err := s.Build(t.Context(), request); !errors.As(err, &waiting) || waiting.Reason != reason {
			t.Fatalf("PR %d: want a %q wait, got %v", request.PullRequestID, reason, err)
		}
		return waiting.Until
	}
	admitted := func(request env.Request) {
		t.Helper()
		if err := s.Build(t.Context(), request); err != nil {
			t.Fatalf("PR %d must be admitted: %v", request.PullRequestID, err)
		}
	}
	// started waits until this request's build is running (other builds may
	// have signalled before it).
	started := func(request env.Request) {
		t.Helper()
		for id := range r.started {
			if id == request.PullRequestID {
				return
			}
		}
	}
	finish := func(requests ...env.Request) {
		t.Helper()
		for _, request := range requests {
			exec("UPDATE build_intents SET state='succeeded', completed_at=now() WHERE pull_request_id=$1", request.PullRequestID)
		}
	}

	// One build at a time per account, beside another account's.
	f.repo.WithBuildSlots(2).WithAccountLimits(env.AccountLimits{BuildsPerAccount: 1})
	a1, a2, b1 := f.prEvent(t, 21, "sha-a1"), f.prEvent(t, 22, "sha-a2"), f.prEventIn(t, other, 31, "sha-b1")
	r.gates[a1.PullRequestID] = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- s.Build(t.Context(), a1) }()
	started(a1)
	waits(a2, env.WaitAccount)
	admitted(b1)
	close(r.gates[a1.PullRequestID])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	finish(a1, b1)
	exec("UPDATE build_intents SET state='cancelled', completed_at=now(), waiting_revision=NULL, waiting_reason=NULL WHERE pull_request_id=$1", a2.PullRequestID)

	// Turns: with one slot, the longest-waiting account goes next, even when
	// another request looks first.
	f.repo.WithBuildSlots(1).WithAccountLimits(env.AccountLimits{})
	a3, a4, b2 := f.prEvent(t, 23, "sha-a3"), f.prEvent(t, 24, "sha-a4"), f.prEventIn(t, other, 32, "sha-b2")
	r.gates[a3.PullRequestID] = make(chan struct{})
	go func() { done <- s.Build(t.Context(), a3) }()
	started(a3)
	waits(b2, env.WaitCapacity)
	time.Sleep(10 * time.Millisecond) // a4 starts waiting after b2
	waits(a4, env.WaitCapacity)
	close(r.gates[a3.PullRequestID])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	finish(a3)
	waits(a4, env.WaitCapacity) // b2 waited longer and its account builds nothing: its turn
	admitted(b2)
	finish(b2)
	admitted(a4)
	finish(a4)

	// A daily budget: an account that used its minutes waits until midnight
	// UTC; another account is unaffected.
	f.repo.WithBuildSlots(2).WithAccountLimits(env.AccountLimits{MinutesPerDay: 1})
	var owner int64
	if err := f.pool.QueryRow(t.Context(), "SELECT i.owner_user_id FROM installations i JOIN repos r ON r.installation_id=i.id WHERE r.id=$1", f.repoID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO account_build_usage(user_id,day,build_seconds,builds) VALUES($1,(now() AT TIME ZONE 'UTC')::date,60,1)
		ON CONFLICT (user_id,day) DO UPDATE SET build_seconds=60`, owner)
	a5, b3 := f.prEvent(t, 25, "sha-a5"), f.prEventIn(t, other, 33, "sha-b3")
	until := waits(a5, env.WaitQuota).UTC()
	now := time.Now().UTC()
	if !until.After(now) || until.Sub(now) > 24*time.Hour || until.Hour() != 0 || until.Minute() != 0 {
		t.Fatalf("a used budget waits until midnight UTC, got %v", until)
	}
	admitted(b3)
	finish(b3)

	// A finished build counts its time for its account and wakes the next
	// request in line: a5's snoozed job becomes due now.
	exec("UPDATE build_intents SET waiting_checked_at=now() WHERE pull_request_id=$1", a5.PullRequestID)
	b4 := f.prEventIn(t, other, 34, "sha-b4")
	if state, _ := runBuildJob(t, f, s, b4); state != "completed" {
		t.Fatalf("b4's job ended %s", state)
	}
	var builds int
	if err := f.pool.QueryRow(t.Context(), "SELECT builds FROM account_build_usage WHERE user_id=$1", otherOwner).Scan(&builds); err != nil || builds != 1 {
		t.Fatalf("the finished build's time is not counted for its account: %d %v", builds, err)
	}
}
