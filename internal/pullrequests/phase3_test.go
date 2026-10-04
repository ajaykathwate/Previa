package pullrequests_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/webhooks"
	"github.com/google/go-github/v91/github"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type testInstallationClient struct{ client *github.Client }

func (c testInstallationClient) InstallationClient(int64) (*github.Client, error) {
	return c.client, nil
}

func TestConnectionStoresOnlyTwoOpenPRsAndNeverHistoricalClosures(t *testing.T) {
	f := setup(t)
	var queries []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repositories/100" {
			if _, err := fmt.Fprint(w, `{"id":100,"name":"repo","owner":{"login":"owner"}}`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path != "/repos/owner/repo/pulls" {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("state") != "open" {
			t.Error("historical API query", r.URL.RawQuery)
		}
		// Include closed/merged records even with state=open to prove the persistence
		// boundary also filters an unexpected provider response.
		rows := []map[string]any{}
		for n, state := range []string{"open", "open", "closed", "closed"} {
			row := map[string]any{"number": n + 1, "state": state, "updated_at": "2026-10-03T10:00:00Z", "mergeable": true, "mergeable_state": "clean", "head": map[string]any{"sha": fmt.Sprintf("sha-%d", n+1), "repo": map[string]any{"id": 100}}}
			if n == 3 {
				row["merged_at"] = "2026-10-03T09:00:00Z"
			}
			rows = append(rows, row)
		}
		if err := json.NewEncoder(w).Encode(rows); err != nil {
			t.Error(err)
		}
	}))
	defer api.Close()
	base := api.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(api.Client()), github.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	service := pr.NewSyncService(f.pool, f.producer, pr.NewGitHubSource(testInstallationClient{client}))
	store := installations.NewRepository(f.pool, service)
	if err := store.Link(t.Context(), 99, f.userID, 0, "owner", []installations.RepositoryInfo{{ID: 100, FullName: "owner/repo", Accessible: true}}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM river_job WHERE kind='pr_discovery'") != 1 {
		t.Fatal("connection did not queue initial discovery")
	}
	// A repeated verified callback must not seed a second initial run.
	if err := store.Link(t.Context(), 99, f.userID, 1, "owner", []installations.RepositoryInfo{{ID: 100, FullName: "owner/repo", Accessible: true}}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM river_job WHERE kind='pr_discovery'") != 1 {
		t.Fatal("relink duplicated initial discovery")
	}
	if err := service.RunPage(t.Context(), pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pull_requests") != 2 || f.count(t, "SELECT count(*) FROM pull_requests WHERE state='open'") != 2 {
		t.Fatal("historical PRs stored")
	}
	t.Logf("Connection: 2 open + 1 closed + 1 merged => exactly 2 stored; GET /repos/owner/repo/pulls?%s", queries[0])
	for _, n := range []int{3, 4} {
		b := payload("closed", fmt.Sprintf("sha-%d", n), "closed", "2026-10-03T11:00:00Z")
		var body map[string]any
		if err := json.Unmarshal([]byte(b), &body); err != nil {
			t.Fatal(err)
		}
		body["pull_request"].(map[string]any)["number"] = n
		if n == 4 {
			body["pull_request"].(map[string]any)["merged"] = true
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if send(f.hook, fmt.Sprintf("historical-%d", n), "pull_request", string(encoded)) != 204 {
			t.Fatal("close delivery failed")
		}
	}
	if err := service.Request(t.Context(), f.userID, f.repoID); err != nil {
		t.Fatal(err)
	}
	if err := service.RunPage(t.Context(), pr.SyncArgs{RepoID: f.repoID, Revision: 2, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pull_requests") != 2 {
		t.Fatal("later sync/unknown closure backfilled history")
	}
	// Legacy parsed-event entry point has the same no-insert-on-close rule.
	legacy := webhooks.NewService(f.pool, environments.NewBroker(f.pool, f.producer))
	if _, err := legacy.ProcessPR(t.Context(), webhooks.PREvent{RepoID: f.repoID, Number: 90, HeadSHA: "old", Action: "closed"}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pull_requests") != 2 {
		t.Fatal("legacy close backfilled")
	}
	// A known PR closes/merges in place, preserving its local identity.
	var id int64
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM pull_requests WHERE number=1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	b := payload("closed", "sha-1", "closed", "2026-10-03T12:00:00Z")
	var body map[string]any
	if err := json.Unmarshal([]byte(b), &body); err != nil {
		t.Fatal(err)
	}
	body["pull_request"].(map[string]any)["number"] = 1
	body["pull_request"].(map[string]any)["merged"] = true
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if send(f.hook, "tracked-merge", "pull_request", string(encoded)) != 204 {
		t.Fatal("merge failed")
	}
	var savedID int64
	var state string
	if err := f.pool.QueryRow(t.Context(), "SELECT id,state FROM pull_requests WHERE number=1").Scan(&savedID, &state); err != nil {
		t.Fatal(err)
	}
	if savedID != id || state != "merged" || f.count(t, "SELECT count(*) FROM pull_requests") != 2 {
		t.Fatal("tracked merge lost identity")
	}
}

type pollSource struct {
	get func() (pr.Observation, error)
}

func (s pollSource) Page(context.Context, int64, int64, int32) ([]pr.Observation, int32, error) {
	return nil, 0, nil
}
func (s pollSource) Get(context.Context, int64, int64, int32) (pr.Observation, error) { return s.get() }

func TestNullMergeabilityPollsAndFencesConflictResolution(t *testing.T) {
	f := setup(t)
	policy, err := installations.NewBuildPolicyService(installations.NewRepository(f.pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Set(t.Context(), f.userID, f.repoID, installations.AutomaticBuilds); err != nil {
		t.Fatal(err)
	}
	result := observation(7)
	result.SHA = "sha-conflict"
	result.Mergeable = nil
	result.MergeableState = "unknown"
	result.BuildStatus = "checking"
	service := pr.NewSyncService(f.pool, f.producer, pollSource{func() (pr.Observation, error) { return result, nil }}, environments.NewBroker(f.pool, f.producer))
	hook := webhooks.NewHandler("fixture-secret", webhooks.NewDeliveryService(f.pool, environments.NewBroker(f.pool, f.producer), nil, service), f.log)
	unknown := func(action, sha, stamp string) string {
		var b map[string]any
		if err := json.Unmarshal([]byte(payload(action, sha, "open", stamp)), &b); err != nil {
			t.Fatal(err)
		}
		p := b["pull_request"].(map[string]any)
		p["mergeable"] = nil
		p["mergeable_state"] = "unknown"
		encoded, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	if send(hook, "unknown", "pull_request", unknown("opened", "sha-conflict", "2026-10-03T10:00:00Z")) != 204 {
		t.Fatal("unknown delivery")
	}
	if f.count(t, "SELECT count(*) FROM pull_requests WHERE build_status='checking' AND mergeable IS NULL") != 1 || f.count(t, "SELECT count(*) FROM river_job WHERE kind='pr_mergeability'") != 1 {
		t.Fatal("null treated as buildable or polling not queued")
	}
	args := pr.MergeabilityArgs{RepoID: f.repoID, Number: 7, Generation: 1, SHA: "sha-conflict", Authorize: true}
	worker := &pr.MergeabilityWorker{Service: service}
	if err := worker.Work(t.Context(), &river.Job[pr.MergeabilityArgs]{JobRow: &rivertype.JobRow{Attempt: 1}, Args: args}); err == nil {
		t.Fatal("unresolved poll completed")
	}
	no := false
	result.Mergeable = &no
	result.MergeableState = "dirty"
	result.BuildStatus = "conflicts"
	if err := service.CheckMergeability(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	row, err := pr.NewRepository(f.pool).Detail(t.Context(), f.userID, f.repoID, 7)
	if err != nil || row.BuildStatus != "conflicts" || row.Mergeable.Bool {
		t.Fatal("conflict not stored", err)
	}
	if err := environments.BuildBlockReason(row.BuildStatus, row.NeedsRefresh); !errors.Is(err, environments.ErrPRConflicts) {
		t.Fatal("conflict not refused", err)
	}
	if send(hook, "resolve-push", "pull_request", unknown("synchronize", "sha-resolved", "2026-10-03T10:01:00Z")) != 204 {
		t.Fatal("resolution push")
	}
	if err := service.CheckMergeability(t.Context(), args); err != nil {
		t.Fatal("obsolete poll", err)
	}
	if f.count(t, "SELECT count(*) FROM pull_requests WHERE build_status='checking' AND head_sha='sha-resolved'") != 1 {
		t.Fatal("old conflict poll won")
	}
	yes := true
	result.SHA = "sha-resolved"
	result.Mergeable = &yes
	result.MergeableState = "clean"
	result.BuildStatus = "open"
	result.UpdatedAt = result.UpdatedAt.Add(60 * 1e9)
	args.Generation = 3
	args.SHA = "sha-resolved"
	args.Authorize = false // A sync check must preserve existing event authorization.
	if err := service.CheckMergeability(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pull_requests WHERE build_status='open' AND mergeable AND head_sha='sha-resolved'") != 1 {
		t.Fatal("resolution not buildable")
	}
	if f.count(t, "SELECT count(*) FROM build_intents b JOIN pull_requests p ON p.id=b.pull_request_id WHERE b.generation=p.generation AND b.state='pending'") != 1 {
		t.Fatal("resolution lost event-driven automatic authorization")
	}
	t.Log("null -> Checking -> dirty/false -> Conflicts; synchronize -> Checking -> clean/true -> Open")
}

func TestDiscoveryPersistenceRejectsHistoricalSnapshots(t *testing.T) {
	f := setup(t)
	source := sourceFunc(func(context.Context, int64, int64, int32) ([]pr.Observation, int32, error) {
		rows := []pr.Observation{observation(1), observation(2), observation(3), observation(4)}
		rows[2].State = "closed"
		rows[3].State = "merged"
		return rows, 0, nil
	})
	service := pr.NewSyncService(f.pool, f.producer, source)
	if err := service.Request(t.Context(), f.userID, f.repoID); err != nil {
		t.Fatal(err)
	}
	if err := service.RunPage(t.Context(), pr.SyncArgs{RepoID: f.repoID, Revision: 1, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM pull_requests") != 2 || f.count(t, "SELECT fetched FROM pr_syncs") != 2 {
		t.Fatal("persistence boundary stored/counts historical snapshots")
	}
}
