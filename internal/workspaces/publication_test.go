package workspaces

import (
	"testing"

	"github.com/ajaykathwatee/previa/internal/environments"
	envdb "github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
)

func pendingPublication(t *testing.T, f fixture, state string) environments.Operation {
	t.Helper()
	ctx := t.Context()
	if _, err := f.pool.Exec(ctx, "UPDATE pull_requests SET build_status='open' WHERE id=$1", f.pr); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,requested_by,idempotency_key,state,available_at,started_at,completed_at)
 VALUES($1,1,0,1,'sha','manual','build',$2,'publication-test',$3,now(),now()-interval '1 minute',CASE WHEN $3='succeeded' THEN now() ELSE NULL END)`, f.pr, f.dev, state); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO environments(pull_request_id,subdomain,status,container_id,image_tag,head_sha,applied_generation,applied_revision,pending_container_id,pending_image_tag,pending_head_sha,pending_generation,pending_revision)
 VALUES($1,'publication-test','publishing','previous','previous-image','old',0,0,'candidate','candidate-image','sha',1,1)`, f.pr); err != nil {
		t.Fatal(err)
	}
	if state == "succeeded" {
		if _, err := f.pool.Exec(ctx, "INSERT INTO workspace_build_usage(workspace_id,day,build_seconds,builds) VALUES($1,current_date,60,1)", f.wid); err != nil {
			t.Fatal(err)
		}
	}
	request := environments.NewRequest(f.pr, 1, "sha", environments.StateOpen)
	request.Revision = 1
	return environments.Operation{Kind: environments.ActivateRoute, PullRequestID: f.pr, Generation: 1, Revision: 1,
		Payload: environments.OperationPayload{Request: request, Artifact: environments.Artifact{ContainerID: "candidate", ImageTag: "candidate-image"}}}
}

func publicationTarget(op environments.Operation) environments.RouteTarget {
	return environments.RouteTarget{Artifact: op.Payload.Artifact, Backend: "candidate-backend", Port: 8080, NetworkID: "candidate-network"}
}

func removeDeveloper(t *testing.T, f fixture) {
	t.Helper()
	w, err := f.service.Read(t.Context(), f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	m, err := dbgen.New(f.pool).LockMembership(t.Context(), dbgen.LockMembershipParams{WorkspaceID: f.wid, UserID: f.dev})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ChangeMember(t.Context(), MemberChange{ActorID: f.owner, WorkspaceID: f.wid, WorkspaceRevision: w.Revision, UserID: f.dev, Revision: m.Revision, Remove: true}); err != nil {
		t.Fatal(err)
	}
}

func TestOffboardingFencesUnacceptedCandidatesAndPreservesAcceptedPreviews(t *testing.T) {
	for _, state := range []string{"running", "succeeded"} {
		for _, accepted := range []bool{false, true} {
			t.Run(state+"/accepted="+map[bool]string{false: "false", true: "true"}[accepted], func(t *testing.T) {
				f := newFixture(t)
				f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
				op := pendingPublication(t, f, state)
				repo := environments.NewRepository(f.pool)
				if accepted {
					if d, err := repo.AcceptRoute(t.Context(), op, publicationTarget(op)); err != nil || !d.Accepted {
						t.Fatalf("accept candidate: %+v %v", d, err)
					}
				}
				removeDeveloper(t, f)
				assertPublicationAfterRemoval(t, f, op, state, accepted)
			})
		}
	}
}

func assertPublicationAfterRemoval(t *testing.T, f fixture, op environments.Operation, state string, accepted bool) {
	t.Helper()
	ctx := t.Context()
	var actualState, active, pending string
	var generation int64
	if err := f.pool.QueryRow(ctx, `SELECT b.state,p.generation,e.container_id,coalesce(e.pending_container_id,'')
 FROM pull_requests p JOIN build_intents b ON b.pull_request_id=p.id JOIN environments e ON e.pull_request_id=p.id WHERE p.id=$1`, f.pr).Scan(&actualState, &generation, &active, &pending); err != nil {
		t.Fatal(err)
	}
	if active != "previous" {
		t.Fatal("offboarding changed the previously serving artifact", active)
	}
	if accepted {
		if actualState != state || generation != 1 || pending != "candidate" {
			t.Fatalf("accepted preview interrupted: %s %d %s", actualState, generation, pending)
		}
		if state == "running" {
			q := envdb.New(f.pool)
			n, err := q.FinishBuildIntent(ctx, envdb.FinishBuildIntentParams{PrID: f.pr, Generation: 1, Revision: 1, NextState: "succeeded"})
			if err != nil || n != 1 {
				t.Fatal("accepted build could not release its slot", n, err)
			}
			if err := q.RecordFinishedBuildTime(ctx, envdb.RecordFinishedBuildTimeParams{PrID: f.pr, Revision: 1}); err != nil {
				t.Fatal(err)
			}
		}
	} else if actualState != "cancelled" || generation != 2 || pending != "" {
		t.Fatalf("unaccepted candidate retained: %s %d %s", actualState, generation, pending)
	}
	d, err := environments.NewRepository(f.pool).AcceptRoute(ctx, op, publicationTarget(op))
	if err != nil || d.Accepted != accepted {
		t.Fatalf("publication after removal: %+v %v", d, err)
	}
	resources := []environments.Resource{
		{PullRequestID: f.pr, Generation: 0, Revision: 0, Artifact: environments.Artifact{ContainerID: "previous", ImageTag: "previous-image"}},
		{PullRequestID: f.pr, Generation: 1, Revision: 1, Artifact: op.Payload.Artifact},
	}
	cleanup, err := environments.NewRepository(f.pool).PlanResourceCleanup(ctx, op.Payload.Request, resources)
	if err != nil {
		t.Fatal(err)
	}
	if accepted && len(cleanup) != 0 || !accepted && (len(cleanup) != 1 || cleanup[0].Artifact != op.Payload.Artifact) {
		t.Fatalf("offboarding cleanup did not preserve the route and release only the revoked candidate: %+v", cleanup)
	}
	var builds int
	var seconds int64
	if err := f.pool.QueryRow(ctx, "SELECT builds,build_seconds FROM workspace_build_usage WHERE workspace_id=$1", f.wid).Scan(&builds, &seconds); err != nil || builds != 1 || seconds < 60 {
		t.Fatalf("lost or duplicated usage: %d %d %v", builds, seconds, err)
	}
}

func TestReviewerKeepsPreviousPreviewLinkDuringReplacement(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	if _, err := f.pool.Exec(t.Context(), "INSERT INTO environments(pull_request_id,subdomain,status,container_id,image_tag,head_sha) VALUES($1,'previous-preview','publishing','previous','previous-image','old')", f.pr); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "publishing", "failed"} {
		if _, err := f.pool.Exec(t.Context(), "UPDATE environments SET status=$1 WHERE pull_request_id=$2", status, f.pr); err != nil {
			t.Fatal(err)
		}
		p, err := f.service.Page(t.Context(), f.reviewer, f.wid, "previews", "preview.test", 1)
		if err != nil || len(p.Previews) != 1 || p.Previews[0].URL != "https://previous-preview.preview.test" {
			t.Fatalf("previous preview link lost during %s: %+v %v", status, p.Previews, err)
		}
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE repos SET accessible=false WHERE id=$1", f.repo); err != nil {
		t.Fatal(err)
	}
	p, err := f.service.Page(t.Context(), f.reviewer, f.wid, "previews", "preview.test", 1)
	if err != nil || len(p.Previews) != 1 || p.Previews[0].URL != "" {
		t.Fatalf("unavailable repository exposed a preview link: %+v %v", p.Previews, err)
	}
}

func TestRouteFenceRechecksActorEvenWithoutGenerationChange(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	op := pendingPublication(t, f, "succeeded")
	if _, err := f.pool.Exec(t.Context(), "UPDATE workspace_memberships SET state='removed',revision=revision+1 WHERE workspace_id=$1 AND user_id=$2", f.wid, f.dev); err != nil {
		t.Fatal(err)
	}
	if d, err := environments.NewRepository(f.pool).AcceptRoute(t.Context(), op, publicationTarget(op)); err != nil || d.Accepted {
		t.Fatalf("revoked actor passed route fence: %+v %v", d, err)
	}
}

func TestSavingUnchangedAccessPreservesManualRequest(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	_ = pendingPublication(t, f, "running")
	w, err := f.service.Read(t.Context(), f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	m, err := dbgen.New(f.pool).LockMembership(t.Context(), dbgen.LockMembershipParams{WorkspaceID: f.wid, UserID: f.dev})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.ChangeMember(t.Context(), MemberChange{ActorID: f.owner, WorkspaceID: f.wid, WorkspaceRevision: w.Revision, UserID: f.dev, Revision: m.Revision, Role: "developer", Repositories: []int64{f.repo, f.repo}}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := f.service.AuthorizeBuild(t.Context(), f.pr); err != nil || !allowed {
		t.Fatal("unchanged grants cancelled work", allowed, err)
	}
}
