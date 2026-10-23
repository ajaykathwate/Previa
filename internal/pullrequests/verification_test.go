package pullrequests_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/jackc/pgx/v5"
)

// upstreamFake serves discovery pages (unused here) and targeted verification reads.
type upstreamFake struct {
	calls   int
	inspect func(ctx context.Context, installationID, repoID int64, number int32) (pr.Upstream, error)
}

func (*upstreamFake) Page(context.Context, int64, int64, int32) ([]pr.Observation, int32, error) {
	return nil, 0, errors.New("discovery is not part of verification")
}
func (u *upstreamFake) Inspect(ctx context.Context, installationID, repoID int64, number int32) (pr.Upstream, error) {
	u.calls++
	return u.inspect(ctx, installationID, repoID, number)
}

var verifyBase = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

func upstreamPR(sha, state string, mergeable *bool, mergeableState string, at time.Time) pr.Upstream {
	return pr.Upstream{Owner: "renamed-owner", Repository: "renamed-repo", Observation: pr.Observation{
		Number: 7, SHA: sha, State: state, Title: "Improve preview", Author: "contributor", Branch: "feature",
		HeadRepoID: 100, Mergeable: mergeable, MergeableState: mergeableState, UpdatedAt: at,
	}}
}

func seedPR(t *testing.T, f *fixture, mergeable string) int64 {
	t.Helper()
	body := fmt.Sprintf(`{"action":"opened","installation":{"id":99},"repository":{"id":100},"pull_request":{"number":7,"title":"Improve preview","state":"open",%s"updated_at":%q,"user":{"login":"contributor"},"html_url":"https://github.com/owner/repo/pull/7","head":{"sha":"sha-a","ref":"feature","repo":{"id":100}}}}`, mergeable, verifyBase.Format(time.RFC3339))
	if code := send(f.hook, fmt.Sprintf("seed-%d", time.Now().UnixNano()), "pull_request", body); code != 204 {
		t.Fatalf("seed webhook: %d", code)
	}
	var id int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM pull_requests WHERE number=7").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func prRow(t *testing.T, f *fixture) (generation int64, sha, state, status string) {
	t.Helper()
	if err := f.pool.QueryRow(t.Context(), "SELECT generation, head_sha, state, build_status FROM pull_requests WHERE number=7").Scan(&generation, &sha, &state, &status); err != nil {
		t.Fatal(err)
	}
	return
}

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

func TestVerifyUpstreamRecordsFreshStateWithoutAuthorizing(t *testing.T) {
	f := setup(t)
	id := seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
	generation, _, _, _ := prRow(t, f)
	fake := &upstreamFake{inspect: func(_ context.Context, installationID, repoID int64, number int32) (pr.Upstream, error) {
		if installationID != 99 || repoID != 100 || number != 7 {
			t.Errorf("verification used wrong identity: %d/%d/#%d", installationID, repoID, number)
		}
		return upstreamPR("sha-a", "open", yes(), "clean", verifyBase), nil
	}}
	service := pr.NewSyncService(f.pool, f.producer, fake, environments.NewBroker(f.pool, f.producer))
	snapshot, err := service.VerifyUpstream(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := environments.UpstreamSnapshot{PullRequestID: id, Number: 7, Generation: generation, HeadSHA: "sha-a", InstallationID: 99, BaseRepoID: 100, HeadRepoID: 100, Fork: false, Owner: "renamed-owner", Repository: "renamed-repo"}
	if snapshot != want {
		t.Fatalf("snapshot = %+v\nwant %+v", snapshot, want)
	}
	if !snapshot.Matches(environments.Request{PullRequestID: id, Generation: generation, HeadSHA: "sha-a", State: "open"}) {
		t.Fatal("current request must match its fresh snapshot")
	}
	if f.count(t, "SELECT count(*) FROM build_intents") != 0 {
		t.Fatal("verification authorized a Manual build")
	}

	// The upstream head moved: the new head is recorded, the old request no longer
	// matches, and Manual mode still authorizes nothing.
	fake.inspect = func(context.Context, int64, int64, int32) (pr.Upstream, error) {
		return upstreamPR("sha-b", "open", yes(), "clean", verifyBase.Add(time.Minute)), nil
	}
	moved, err := service.VerifyUpstream(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if moved.HeadSHA != "sha-b" || moved.Generation != generation+1 || moved.Matches(environments.Request{PullRequestID: id, Generation: generation, HeadSHA: "sha-a", State: "open"}) {
		t.Fatalf("moved head not recorded as a newer generation: %+v", moved)
	}
	if f.count(t, "SELECT count(*) FROM build_intents") != 0 {
		t.Fatal("verification of a new head authorized a Manual build")
	}
}

func TestVerifyUpstreamFailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		upstream   pr.Upstream
		err        error
		want       error
		wantState  string
		wantStatus string
		retryable  bool
	}{
		{name: "closed upstream is recorded", upstream: upstreamPR("sha-a", "closed", yes(), "clean", verifyBase.Add(time.Minute)), want: environments.ErrUpstreamClosed, wantState: "closed", wantStatus: "open"},
		{name: "conflicts", upstream: upstreamPR("sha-a", "open", no(), "dirty", verifyBase), want: environments.ErrPRConflicts, wantState: "open", wantStatus: "conflicts"},
		{name: "unresolved mergeability", upstream: upstreamPR("sha-a", "open", nil, "unknown", verifyBase), want: environments.ErrPRChecking, wantState: "open", wantStatus: "checking", retryable: true},
		{name: "inaccessible", err: environments.ErrUpstreamInaccessible, want: environments.ErrUpstreamInaccessible, wantState: "open", wantStatus: "open"},
		{name: "unavailable", err: environments.ErrUpstreamUnavailable, want: environments.ErrUpstreamUnavailable, wantState: "open", wantStatus: "open", retryable: true},
		{name: "rate limited", err: &environments.UpstreamRetryAfter{Delay: time.Minute, Err: environments.ErrUpstreamUnavailable}, want: environments.ErrUpstreamUnavailable, wantState: "open", wantStatus: "open", retryable: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			id := seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
			fake := &upstreamFake{inspect: func(context.Context, int64, int64, int32) (pr.Upstream, error) { return tt.upstream, tt.err }}
			service := pr.NewSyncService(f.pool, f.producer, fake, environments.NewBroker(f.pool, f.producer))
			_, err := service.VerifyUpstream(t.Context(), id)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v; want %v", err, tt.want)
			}
			if environments.UpstreamRetryable(err) != tt.retryable {
				t.Fatalf("retryable = %v; want %v", environments.UpstreamRetryable(err), tt.retryable)
			}
			if _, _, state, status := prRow(t, f); state != tt.wantState || status != tt.wantStatus {
				t.Fatalf("recorded %s/%s; want %s/%s", state, status, tt.wantState, tt.wantStatus)
			}
			if tt.want == environments.ErrPRChecking && f.count(t, "SELECT count(*) FROM river_job WHERE kind='pr_mergeability'") == 0 {
				t.Fatal("unresolved mergeability did not schedule a recheck")
			}
			if tt.want == environments.ErrUpstreamClosed {
				calls := fake.calls
				if _, err := service.VerifyUpstream(t.Context(), id); !errors.Is(err, environments.ErrUpstreamClosed) || fake.calls != calls {
					t.Fatal("a locally closed PR must fail closed without another GitHub read")
				}
			}
		})
	}
}

func TestVerifyUpstreamRejectsChangesDuringIO(t *testing.T) {
	cases := []struct {
		name   string
		during func(t *testing.T, f *fixture)
		want   error
	}{
		{"installation revision", func(t *testing.T, f *fixture) {
			if _, err := f.pool.Exec(t.Context(), "UPDATE installations SET revision=revision+1"); err != nil {
				t.Error(err)
			}
		}, environments.ErrUpstreamChanged},
		{"concurrent webhook", func(t *testing.T, f *fixture) {
			if code := send(f.hook, "during-io", "pull_request", payload("synchronize", "sha-c", "open", verifyBase.Add(2*time.Minute).Format(time.RFC3339))); code != 204 {
				t.Errorf("webhook during IO: %d", code)
			}
		}, environments.ErrUpstreamChanged},
		{"access revoked", func(t *testing.T, f *fixture) {
			if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET accessible=false"); err != nil {
				t.Error(err)
			}
		}, environments.ErrUpstreamInaccessible},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			id := seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
			fake := &upstreamFake{inspect: func(context.Context, int64, int64, int32) (pr.Upstream, error) {
				tt.during(t, f)
				return upstreamPR("sha-b", "open", yes(), "clean", verifyBase.Add(time.Minute)), nil
			}}
			service := pr.NewSyncService(f.pool, f.producer, fake, environments.NewBroker(f.pool, f.producer))
			if _, err := service.VerifyUpstream(t.Context(), id); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v; want %v", err, tt.want)
			}
			if _, sha, _, _ := prRow(t, f); sha == "sha-b" {
				t.Fatal("a fenced-out verification wrote its stale upstream view")
			}
		})
	}
}

func TestVerifyUpstreamPreservesEventAuthorizedAutomaticWork(t *testing.T) {
	f := setup(t)
	policy, err := installations.NewBuildPolicyService(installations.NewRepository(f.pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Set(t.Context(), f.userID, f.repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	// An unresolved webhook observation still records Automatic intent; the
	// verified resolution must carry it to the new generation, not drop it.
	id := seedPR(t, f, "")
	if f.count(t, "SELECT count(*) FROM build_intents WHERE source='automatic' AND state='pending'") != 1 {
		t.Fatal("fixture expected event-authorized automatic intent")
	}
	fake := &upstreamFake{inspect: func(context.Context, int64, int64, int32) (pr.Upstream, error) {
		return upstreamPR("sha-a", "open", yes(), "clean", verifyBase), nil
	}}
	service := pr.NewSyncService(f.pool, f.producer, fake, environments.NewBroker(f.pool, f.producer))
	snapshot, err := service.VerifyUpstream(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var generation int64
	var source, state string
	if err := f.pool.QueryRow(t.Context(), "SELECT generation, source, state FROM build_intents WHERE pull_request_id=$1", id).Scan(&generation, &source, &state); err != nil {
		t.Fatal(err)
	}
	if generation != snapshot.Generation || source != "automatic" || state != "pending" {
		t.Fatalf("automatic intent not carried forward: generation %d (snapshot %d), %s/%s", generation, snapshot.Generation, source, state)
	}
}

// reportingObserver records which PRs were reported, like the environments
// broker with reports enabled.
type reportingObserver struct{ reported []int64 }

func (o *reportingObserver) ObserveTx(_ context.Context, _ pgx.Tx, r environments.Request) (environments.Request, error) {
	return r, nil
}
func (o *reportingObserver) RecordReportTx(_ context.Context, _ pgx.Tx, pullRequestID int64) error {
	o.reported = append(o.reported, pullRequestID)
	return nil
}

func TestRefreshedObservationsRecordReportsOnlyWhenTheyChangeThePR(t *testing.T) {
	f := setup(t)
	id := seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
	observer := &reportingObserver{}
	current := upstreamPR("sha-a", "open", yes(), "clean", verifyBase)
	fake := &upstreamFake{inspect: func(context.Context, int64, int64, int32) (pr.Upstream, error) { return current, nil }}
	service := pr.NewSyncService(f.pool, f.producer, fake, observer)
	if _, err := service.VerifyUpstream(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if len(observer.reported) != 0 {
		t.Fatalf("an unchanged PR was reported: %v", observer.reported)
	}
	// A missed close, found by verification, must reach the PR's report.
	current = upstreamPR("sha-a", "closed", yes(), "clean", verifyBase.Add(time.Minute))
	if _, err := service.VerifyUpstream(t.Context(), id); !errors.Is(err, environments.ErrUpstreamClosed) {
		t.Fatalf("closed upstream: %v", err)
	}
	if len(observer.reported) != 1 || observer.reported[0] != id {
		t.Fatalf("reported %v, want [%d]", observer.reported, id)
	}
}

// listingFake lists one page of open PRs, as GitHub would for a sync.
type listingFake struct{ observations []pr.Observation }

func (l *listingFake) Page(context.Context, int64, int64, int32) ([]pr.Observation, int32, error) {
	return l.observations, 0, nil
}

// A missed push: a newer commit on a tracked open PR, found by housekeeping's
// confirmation or Sync Pull Requests, is scheduled in Automatic mode after the
// quiet period, as the lost webhook would have done. The check before a manual
// build, connection discovery and Manual mode never start work, and switching
// to Automatic builds nothing by itself.
func TestConfirmedNewCommitIsScheduledLikeAPush(t *testing.T) {
	automatic := func(t *testing.T, f *fixture) {
		t.Helper()
		policy, err := installations.NewBuildPolicyService(installations.NewRepository(f.pool))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := policy.Set(t.Context(), f.userID, f.repoID, installations.AutomaticBuilds); err != nil {
			t.Fatal(err)
		}
	}
	pending := func(t *testing.T, f *fixture) (sha string, wait time.Duration, n int) {
		t.Helper()
		n = f.count(t, "SELECT count(*) FROM build_intents WHERE source='automatic' AND state='pending'")
		if n == 1 {
			if err := f.pool.QueryRow(t.Context(), "SELECT head_sha, available_at - now() FROM build_intents").Scan(&sha, &wait); err != nil {
				t.Fatal(err)
			}
		}
		return sha, wait, n
	}
	newer := upstreamPR("sha-b", "open", yes(), "clean", verifyBase.Add(time.Minute))
	confirm := &upstreamFake{inspect: func(context.Context, int64, int64, int32) (pr.Upstream, error) { return newer, nil }}

	t.Run("housekeeping confirms a new commit in Automatic", func(t *testing.T) {
		f := setup(t)
		id := seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
		automatic(t, f)
		if _, _, n := pending(t, f); n != 0 {
			t.Fatal("switching to Automatic must not build existing PRs")
		}
		service := pr.NewSyncService(f.pool, f.producer, confirm, environments.NewBroker(f.pool, f.producer))
		if _, err := service.ConfirmUpstream(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		sha, wait, n := pending(t, f)
		if n != 1 || sha != "sha-b" || wait < 4*time.Minute || wait > environments.QuietPeriod {
			t.Fatalf("a confirmed new commit must wait out the quiet period: %d %s %s", n, sha, wait)
		}
	})
	t.Run("the check before a manual build starts nothing", func(t *testing.T) {
		f := setup(t)
		id := seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
		automatic(t, f)
		service := pr.NewSyncService(f.pool, f.producer, confirm, environments.NewBroker(f.pool, f.producer))
		if _, err := service.VerifyUpstream(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if _, _, n := pending(t, f); n != 0 {
			t.Fatal("verification before a manual request must not schedule Automatic work")
		}
	})
	t.Run("Manual mode schedules nothing", func(t *testing.T) {
		f := setup(t)
		id := seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
		service := pr.NewSyncService(f.pool, f.producer, confirm, environments.NewBroker(f.pool, f.producer))
		if _, err := service.ConfirmUpstream(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if f.count(t, "SELECT count(*) FROM build_intents") != 0 {
			t.Fatal("Manual mode builds only on request")
		}
	})
	for _, c := range []struct {
		name    string
		confirm bool
		want    int
	}{{"Sync Pull Requests schedules the new commit", true, 1}, {"connection discovery only records it", false, 0}} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			seedPR(t, f, `"mergeable":true,"mergeable_state":"clean",`)
			automatic(t, f)
			service := pr.NewSyncService(f.pool, f.producer, &listingFake{observations: []pr.Observation{newer.Observation}}, environments.NewBroker(f.pool, f.producer))
			if err := service.Request(t.Context(), f.userID, f.repoID); err != nil {
				t.Fatal(err)
			}
			if err := service.RunPage(t.Context(), pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1, Confirm: c.confirm}); err != nil {
				t.Fatal(err)
			}
			if sha, _, n := pending(t, f); n != c.want || (n == 1 && sha != "sha-b") {
				t.Fatalf("pending automatic intents %d (%s), want %d", n, sha, c.want)
			}
			if got := f.count(t, "SELECT count(*) FROM pull_requests WHERE head_sha='sha-b'"); got != 1 {
				t.Fatal("the new commit must be recorded either way")
			}
		})
	}
}
