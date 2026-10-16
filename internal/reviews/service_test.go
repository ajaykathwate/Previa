package reviews

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	pages "github.com/ajaykathwatee/previa/web/pages/reviews"
)

func TestReviewIsForPeopleWithPreviewAccess(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for name, user := range map[string]int64{"owner": f.owner, "admin": f.admin, "granted developer": f.dev, "granted reviewer": f.reviewer} {
		p, err := f.service.Page(ctx, user, f.repo, 42, Query{})
		if err != nil || p.Title != "Change 42" || p.Repository != "acme/api" {
			t.Errorf("%s: %v %+v", name, err, p)
		}
		if want := user != f.reviewer; p.CanOperate != want {
			t.Errorf("%s may operate: %v", name, p.CanOperate)
		}
	}
	for name, attempt := range map[string][2]int64{
		"outsider": {f.outsider, f.repo}, "developer without a grant": {f.newdev, f.repo}, "reviewer without a grant": {f.reviewer, f.otherRepo},
		"another workspace's repository": {f.owner, f.outsideRepo},
	} {
		if _, err := f.service.Page(ctx, attempt[0], attempt[1], 42, Query{}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.service.CreateFinding(ctx, Actor{UserID: f.outsider, RepoID: f.repo, Number: 42}, FindingInput{Body: "x", RequestKey: key(1)}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an outsider cannot write: %v", err)
	}
	if _, err := f.service.CreateFinding(ctx, Actor{UserID: f.reviewer, RepoID: f.otherRepo, Number: 7}, FindingInput{Body: "x", RequestKey: key(2)}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a forged repository: %v", err)
	}
}

func TestAFindingsConversation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	first := f.publish(t, f.pr, shaA)
	number, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{
		Body: "  An empty address crashes checkout.\r\nExpected an inline error.  ", Path: "/checkout?step=2",
		Details: "1. Open /checkout\n2. Leave the address empty", RequestKey: key(1), Deployment: first,
	})
	if err != nil || number != 1 {
		t.Fatalf("create: %d %v", number, err)
	}
	again, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "A different text", RequestKey: key(1), Deployment: first})
	if err != nil || again != 1 {
		t.Fatalf("a repeated submission returns the first finding: %d %v", again, err)
	}
	reply, err := f.service.Reply(ctx, f.as(f.dev), 1, ReplyInput{Body: "Fixed in the next commit.", RequestKey: key(2), Deployment: first})
	if err != nil || reply != 2 {
		t.Fatalf("reply: %d %v", reply, err)
	}
	if again, err := f.service.Reply(ctx, f.as(f.dev), 1, ReplyInput{Body: "Fixed in the next commit.", RequestKey: key(2)}); err != nil || again != 2 {
		t.Fatalf("a repeated reply: %d %v", again, err)
	}
	p, err := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Findings.Open != 1 || len(p.Findings.Threads) != 1 || p.OpenFindings != 1 {
		t.Fatalf("findings: %+v", p.Findings)
	}
	th := p.Findings.Threads[0]
	opening := th.Opening()
	if opening.Body != "An empty address crashes checkout.\nExpected an inline error." || opening.Path != "/checkout?step=2" || !strings.HasPrefix(opening.Details, "1. Open") {
		t.Fatalf("opening message: %+v", opening)
	}
	if th.Deployment == nil || th.Deployment.Sequence != first || th.Author.Name != "Ravi Reviewer" || !th.Author.You {
		t.Fatalf("finding context: %+v", th)
	}
	if !th.CanResolve || th.CanRemove || !opening.CanEdit || !opening.CanRemove || opening.Moderate {
		t.Fatalf("the author's actions: %+v / %+v", th, opening)
	}
	if replies := th.Replies(); len(replies) != 1 || replies[0].CanEdit || replies[0].CanRemove || replies[0].Author.Name != "Maya Developer" {
		t.Fatalf("a teammate's reply is not the reviewer's to change: %+v", replies)
	}

	// Edits: only the author's own, compared by revision; the deployment stays.
	if err := f.service.EditMessage(ctx, f.as(f.dev), 1, 1, EditInput{Body: "Hijacked", Revision: 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("someone else's message: %v", err)
	}
	if err := f.service.EditMessage(ctx, f.as(f.reviewer), 1, 1, EditInput{Body: "An empty address crashes the checkout.", Path: "/checkout", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	var conflict *ConflictError
	if err := f.service.EditMessage(ctx, f.as(f.reviewer), 1, 1, EditInput{Body: "Another wording", Revision: 1}); !errors.As(err, &conflict) || conflict.Kind != "edited" || conflict.Revision != 2 {
		t.Fatalf("a stale edit: %v", err)
	}
	if err := f.service.EditMessage(ctx, f.as(f.reviewer), 1, 1, EditInput{Body: "An empty address crashes the checkout.", Path: "/checkout", Revision: 1}); err != nil {
		t.Fatalf("repeating the saved edit is not a conflict: %v", err)
	}
	f.publish(t, f.pr, shaB)
	p, _ = f.service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	th = p.Findings.Threads[0]
	if th.Opening().EditedAt == "" || th.Opening().Details != "" || th.Deployment.Sequence != first || !th.Older(p.Preview.Current) {
		t.Fatalf("an edit keeps the original deployment: %+v", th)
	}

	// Resolve against the deployment shown and live; reopen; a stale revision.
	if err := f.service.Resolve(ctx, f.as(f.reviewer), 1, th.Revision, 2); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Resolve(ctx, f.as(f.reviewer), 1, th.Revision, 2); err != nil {
		t.Fatalf("resolving twice is not an error: %v", err)
	}
	if err := f.service.Reopen(ctx, f.as(f.reviewer), 1, th.Revision); !errors.Is(err, ErrStale) {
		t.Fatalf("a reopen from an old page: %v", err)
	}
	p, _ = f.service.Page(ctx, f.reviewer, f.repo, 42, Query{State: pages.StateResolved})
	if len(p.Findings.Threads) != 1 || p.Findings.Threads[0].ResolvedOn == nil || p.Findings.Threads[0].ResolvedOn.Sequence != 2 || p.Findings.Resolved != 1 || p.Findings.Open != 0 {
		t.Fatalf("resolved on deployment 2: %+v", p.Findings)
	}
	if err := f.service.Reopen(ctx, f.as(f.dev), 1, p.Findings.Threads[0].Revision); err != nil {
		t.Fatalf("a developer may reopen anyone's finding: %v", err)
	}
	if err := f.service.Resolve(ctx, Actor{UserID: f.reviewer, RepoID: f.repo, Number: 42}, 1, p.Findings.Threads[0].Revision+1, 1); err != nil {
		t.Fatal(err)
	}
	p, _ = f.service.Page(ctx, f.reviewer, f.repo, 42, Query{State: pages.StateAll})
	if r := p.Findings.Threads[0].ResolvedOn; r != nil {
		t.Fatalf("resolving against a deployment no longer served records none: %+v", r)
	}
	want := []string{KindFindingCreated, KindReplyAdded, KindMessageEdited, KindFindingResolved, KindFindingReopened, KindFindingResolved}
	if got := f.events(t, f.pr); !slices.Equal(got, want) {
		t.Fatalf("timeline: %v", got)
	}
}

func TestRemovalAndModeration(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.publish(t, f.pr, shaA)
	for i, body := range []string{"First finding", "Second finding"} {
		if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: body, Details: "secret detail", RequestKey: key(i), Deployment: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.Reply(ctx, f.as(f.dev), 2, ReplyInput{Body: "Looking", RequestKey: key(10)}); err != nil {
		t.Fatal(err)
	}
	// Removing an opening message nobody answered removes the finding.
	whole, err := f.service.RemoveMessage(ctx, f.as(f.reviewer), 1, 1, "")
	if err != nil || !whole {
		t.Fatalf("remove an unanswered finding: %v %v", whole, err)
	}
	// With a reply depending on it, a tombstone stays.
	if whole, err := f.service.RemoveMessage(ctx, f.as(f.reviewer), 2, 1, ""); err != nil || whole {
		t.Fatalf("remove an answered opening message: %v %v", whole, err)
	}
	var body string
	var details *string
	if err := f.pool.QueryRow(ctx, "SELECT body, details FROM preview_feedback_messages m JOIN preview_feedback_threads t ON t.id=m.thread_id WHERE t.number=2 AND m.position=1").Scan(&body, &details); err != nil || body != "" || details != nil {
		t.Fatalf("a removed message is erased, not hidden: %q %v %v", body, details, err)
	}
	// A Reviewer cannot moderate; a Developer must give a reason.
	if _, err := f.service.RemoveMessage(ctx, f.as(f.reviewer), 2, 2, "abuse"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a reviewer moderating: %v", err)
	}
	var input *InputError
	if _, err := f.service.RemoveMessage(ctx, f.as(f.admin), 2, 2, ""); !errors.As(err, &input) || input.Fields["reason"] == "" {
		t.Fatalf("moderation without a reason: %v", err)
	}
	if err := f.service.RemoveFinding(ctx, f.as(f.reviewer), 2, "abuse"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a reviewer removing a finding: %v", err)
	}
	if err := f.service.RemoveFinding(ctx, f.as(f.dev), 2, "sensitive"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RemoveFinding(ctx, f.as(f.dev), 2, "sensitive"); err != nil {
		t.Fatalf("removing twice is not an error: %v", err)
	}
	if _, err := f.service.Reply(ctx, f.as(f.reviewer), 2, ReplyInput{Body: "Hello?", RequestKey: key(11)}); !errors.Is(err, ErrGone) {
		t.Fatalf("replying to a removed finding: %v", err)
	}
	p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{State: pages.StateAll})
	if p.Findings.Open != 0 || p.Findings.All != 2 || len(p.Findings.Threads) != 2 {
		t.Fatalf("removed findings count only under All: %+v", p.Findings)
	}
	removed := p.Findings.Threads[0]
	if removed.Number != 2 || removed.State != pages.StateRemoved || removed.RemovedText() != "Removed by Maya Developer · Contains secrets or personal data" {
		t.Fatalf("moderated tombstone: %+v %q", removed, removed.RemovedText())
	}
	if own := p.Findings.Threads[1]; own.RemovedText() != "Removed by its author" {
		t.Fatalf("own removal: %q", own.RemovedText())
	}
	var leftover int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM preview_feedback_messages WHERE pull_request_id=$1 AND (body<>'' OR details IS NOT NULL)", f.pr).Scan(&leftover); err != nil || leftover != 0 {
		t.Fatalf("every removed text is erased: %d %v", leftover, err)
	}
	var reason string
	if err := f.pool.QueryRow(ctx, "SELECT metadata->>'reason' FROM activity_events WHERE kind=$1 AND (metadata->>'moderated')::boolean", KindFindingRemoved).Scan(&reason); err != nil || reason != "sensitive" {
		t.Fatalf("moderation is on the timeline with its reason: %q %v", reason, err)
	}
}

func TestANewerDeploymentNeverRelabelsAFinding(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var conflict *ConflictError
	// Written before anything was deployed, submitted after deployment 1.
	f.publish(t, f.pr, shaA)
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Typo", RequestKey: key(1)}); !errors.As(err, &conflict) || conflict.Tested != nil || conflict.Current.Sequence != 1 {
		t.Fatalf("no deployment shown: %v %+v", err, conflict)
	}
	f.publish(t, f.pr, shaB)
	_, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Crash", RequestKey: key(2), Deployment: 1})
	if !errors.As(err, &conflict) || conflict.Kind != "version" || conflict.Tested.Sequence != 1 || conflict.Current.Sequence != 2 || conflict.Current.SHA != shaB {
		t.Fatalf("a newer deployment: %v %+v", err, conflict)
	}
	if n, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Crash", RequestKey: key(2), Deployment: 1, AcknowledgeOlder: true}); err != nil || n != 1 {
		t.Fatalf("keeping the tested deployment on purpose: %d %v", n, err)
	}
	if n, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Slow", RequestKey: key(3), Deployment: 2}); err != nil || n != 2 {
		t.Fatalf("the newest deployment: %d %v", n, err)
	}
	var input *InputError
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Future", RequestKey: key(4), Deployment: 9}); !errors.As(err, &input) {
		t.Fatalf("an unknown deployment: %v", err)
	}
	p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	got := map[int32]int32{}
	for _, th := range p.Findings.Threads {
		got[th.Number] = th.Deployment.Sequence
	}
	if got[1] != 1 || got[2] != 2 {
		t.Fatalf("recorded deployments: %v", got)
	}
	if d := p.Findings.Deployments; len(d) != 2 || d[0].Sequence != 2 {
		t.Fatalf("deployment filter options: %+v", d)
	}
	if p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{Deployment: 1}); len(p.Findings.Threads) != 1 || p.Findings.Threads[0].Number != 1 {
		t.Fatalf("filtered by deployment: %+v", p.Findings.Threads)
	}
}

func TestTestingInstructions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	in := InstructionsInput{Summary: "Checkout validates the address", Steps: "Open /checkout\n\n  Leave the address empty  \nChoose Pay", Expected: "An inline error", StartPath: "/checkout", DocsPath: "/docs"}
	if err := f.service.SaveInstructions(ctx, f.as(f.reviewer), in); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a Reviewer cannot rewrite the acceptance criteria: %v", err)
	}
	if err := f.service.SaveInstructions(ctx, f.as(f.dev), in); err != nil {
		t.Fatal(err)
	}
	p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	if i := p.Instructions; i.Revision != 1 || i.UpdatedBy.Name != "Maya Developer" || !slices.Equal(i.StepList(), []string{"Open /checkout", "Leave the address empty", "Choose Pay"}) || i.StartPath != "/checkout" {
		t.Fatalf("saved instructions: %+v", i)
	}
	if p.Description != "Why: <b>faster</b> checkout" {
		t.Fatalf("the GitHub description stays separate and raw for escaping: %q", p.Description)
	}
	// Two people editing at once: the second sees the first's version.
	in.Summary = "Checkout validates every field"
	if err := f.service.SaveInstructions(ctx, f.as(f.admin), InstructionsInput{Summary: "Admin's view", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	var conflict *ConflictError
	in.Revision = 1
	if err := f.service.SaveInstructions(ctx, f.as(f.dev), in); !errors.As(err, &conflict) || conflict.Kind != "instructions" || conflict.Saved.Summary != "Admin's view" || conflict.Revision != 2 {
		t.Fatalf("a concurrent save: %v %+v", err, conflict)
	}
	in.Revision = conflict.Revision
	if err := f.service.SaveInstructions(ctx, f.as(f.dev), in); err != nil {
		t.Fatalf("saving over it on purpose: %v", err)
	}
	if got := f.events(t, f.pr); !slices.Equal(got, []string{KindInstructionsUpdated, KindInstructionsUpdated, KindInstructionsUpdated}) {
		t.Fatalf("instruction changes are timeline events: %v", got)
	}
	var input *InputError
	for name, bad := range map[string]InstructionsInput{
		"a token in a path":   {StartPath: "/reset?token=abc"},
		"an absolute URL":     {StartPath: "https://evil.example/phish"},
		"a protocol-relative": {DocsPath: "//evil.example"},
		"too many steps":      {Steps: strings.Repeat("step\n", MaxSteps+1)},
		"a two-line summary":  {Summary: "one\ntwo"},
		"direction override":  {Expected: "evil \u202e txt.exe"},
	} {
		bad.Revision = 3
		if err := f.service.SaveInstructions(ctx, f.as(f.dev), bad); !errors.As(err, &input) || len(input.Fields) == 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := f.service.SaveInstructions(ctx, f.as(f.dev), InstructionsInput{StartPath: "/search?author=me&q=shoes", Revision: 3}); err != nil {
		t.Fatalf("ordinary parameters are fine: %v", err)
	}
}

func TestAClosedPullRequestIsReadOnly(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.publish(t, f.pr, shaA)
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Before merge", RequestKey: key(1), Deployment: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE pull_requests SET state='merged' WHERE id=$1", f.pr); err != nil {
		t.Fatal(err)
	}
	checks := map[string]error{}
	_, checks["finding"] = f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "After", RequestKey: key(2), Deployment: 1})
	_, checks["reply"] = f.service.Reply(ctx, f.as(f.reviewer), 1, ReplyInput{Body: "After", RequestKey: key(3)})
	checks["edit"] = f.service.EditMessage(ctx, f.as(f.reviewer), 1, 1, EditInput{Body: "Changed", Revision: 1})
	checks["resolve"] = f.service.Resolve(ctx, f.as(f.reviewer), 1, 1, 1)
	checks["instructions"] = f.service.SaveInstructions(ctx, f.as(f.dev), InstructionsInput{Summary: "Late"})
	for name, err := range checks {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after merge: %v", name, err)
		}
	}
	p, err := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	if err != nil || !p.Closed() || p.Findings.Threads[0].CanReply || p.Findings.Threads[0].CanResolve || p.Findings.Threads[0].Opening().CanEdit {
		t.Fatalf("read-only page: %v %+v", err, p.Findings.Threads)
	}
	if !p.Findings.Threads[0].Opening().CanRemove {
		t.Fatal("authors can still take their own text back")
	}
	if _, err := f.service.RemoveMessage(ctx, f.as(f.reviewer), 1, 1, ""); err != nil {
		t.Fatalf("removal stays possible after merge: %v", err)
	}
}

func TestARemovedMemberLosesAccessButStaysAttributed(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Mine", RequestKey: key(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE workspace_memberships SET state='removed', removed_at=now() WHERE user_id=$1", f.reviewer); err != nil {
		t.Fatal(err)
	}
	if err := f.service.EditMessage(ctx, f.as(f.reviewer), 1, 1, EditInput{Body: "Still mine?", Revision: 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a removed member's edit: %v", err)
	}
	if _, err := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a removed member's read: %v", err)
	}
	p, _ := f.service.Page(ctx, f.dev, f.repo, 42, Query{})
	if a := p.Findings.Threads[0].Author; !a.Former || a.Name != "Ravi Reviewer" {
		t.Fatalf("attributed to a former member: %+v", a)
	}
}

func TestReviewLimits(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var input *InputError
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: strings.Repeat("é", MaxMessageBytes), RequestKey: key(1)}); !errors.As(err, &input) || input.Fields["body"] == "" {
		t.Fatalf("an oversized message: %v", err)
	}
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "   ", RequestKey: key(1)}); !errors.As(err, &input) {
		t.Fatalf("an empty message: %v", err)
	}
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "ok", RequestKey: "short"}); !errors.As(err, &input) {
		t.Fatalf("a forged request key: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO pr_review_counters(pull_request_id,threads,messages) VALUES($1,$2,10)", f.pr, MaxThreads); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "One too many", RequestKey: key(2)}); !errors.As(err, &input) || !strings.Contains(input.Message, "limit of 500 findings") {
		t.Fatalf("the finding limit: %v", err)
	}
	if p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{}); !p.Findings.Full {
		t.Fatal("the page says the limit is reached")
	}
	if _, err := f.pool.Exec(ctx, "UPDATE pr_review_counters SET threads=0 WHERE pull_request_id=$1", f.pr); err != nil {
		t.Fatal(err)
	}
	// The hourly allowance counts changes, not repeats, and says when it resets.
	limited, err := NewService(f.pool, nil, Options{WriteAllowance: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := limited.CreateFinding(ctx, f.as(f.dev), FindingInput{Body: "x", RequestKey: key(10 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := limited.CreateFinding(ctx, f.as(f.dev), FindingInput{Body: "x", RequestKey: key(10)}); err != nil {
		t.Fatalf("a repeat spends nothing: %v", err)
	}
	var limit *LimitError
	if _, err := limited.CreateFinding(ctx, f.as(f.dev), FindingInput{Body: "x", RequestKey: key(12)}); !errors.As(err, &limit) || limit.ResetsAt.Before(time.Now()) {
		t.Fatalf("the allowance: %v", err)
	}
	var threads int
	if err := f.pool.QueryRow(ctx, "SELECT threads FROM pr_review_counters WHERE pull_request_id=$1", f.pr).Scan(&threads); err != nil || threads != 2 {
		t.Fatalf("a refused change saves nothing: %d %v", threads, err)
	}
}

func TestFiltersSearchAndPages(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.publish(t, f.pr, shaA)
	for i := range 53 {
		author := f.reviewer
		body := "Finding about the cart"
		if i%2 == 1 {
			author, body = f.dev, "Finding with 100% literal_text"
		}
		if _, err := f.service.CreateFinding(ctx, f.as(author), FindingInput{Body: body, RequestKey: key(i), Deployment: 1}); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	if p.Findings.Total != 53 || p.Findings.Pages != 2 || len(p.Findings.Threads) != pages.PageSize || p.Findings.Threads[0].Number != 53 {
		t.Fatalf("the first page, newest first: %d %d %d", p.Findings.Total, p.Findings.Pages, len(p.Findings.Threads))
	}
	if p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{Page: 9}); p.Findings.Page != 2 || len(p.Findings.Threads) != 3 {
		t.Fatalf("a page past the end shows the last: %d %d", p.Findings.Page, len(p.Findings.Threads))
	}
	if p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{Author: f.dev}); p.Findings.Total != 26 {
		t.Fatalf("by author: %d", p.Findings.Total)
	}
	if p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{Search: "100% LITERAL_"}); p.Findings.Total != 26 {
		t.Fatalf("literal, case-insensitive text: %d", p.Findings.Total)
	}
	if p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{Search: "%"}); p.Findings.Total != 26 {
		t.Fatalf("a %% is not a wildcard: %d", p.Findings.Total)
	}
	if p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{Finding: 7}); len(p.Findings.Threads) != 1 || p.Findings.Threads[0].Number != 7 || p.Findings.Focus != 7 {
		t.Fatalf("a finding's own link: %+v", p.Findings)
	}
	if p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{Finding: 700}); !p.Findings.FocusMissing {
		t.Fatal("a missing finding is said to be missing")
	}
	if len(p.Findings.Authors) != 2 {
		t.Fatalf("author options: %+v", p.Findings.Authors)
	}
}

func TestLongConversationsShowTheLatestReplies(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Start", RequestKey: key(0)}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		if _, err := f.service.Reply(ctx, f.as(f.dev), 1, ReplyInput{Body: "Reply " + string(rune('0'+i)), RequestKey: key(i)}); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := f.service.Page(ctx, f.reviewer, f.repo, 42, Query{})
	th := p.Findings.Threads[0]
	if len(th.Messages) != 1+pages.LatestReplies || th.Earlier != 3 || th.Messages[1].Body != "Reply 4" {
		t.Fatalf("list view: %d messages, %d earlier, %+v", len(th.Messages), th.Earlier, th.Messages[1])
	}
	p, _ = f.service.Page(ctx, f.reviewer, f.repo, 42, Query{Finding: 1})
	if th := p.Findings.Threads[0]; len(th.Messages) != 7 || th.Earlier != 0 {
		t.Fatalf("its own link shows all: %d %d", len(th.Messages), th.Earlier)
	}
}

func TestDeletingAPullRequestTakesItsReviewWithIt(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.publish(t, f.pr, shaA)
	if err := f.service.SaveInstructions(ctx, f.as(f.dev), InstructionsInput{Summary: "Test it"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Found", RequestKey: key(1), Deployment: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Resolve(ctx, f.as(f.reviewer), 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	// Housekeeping deletes a retired pull request's environment, then the pull
	// request; everything of its review goes in the same statement.
	if _, err := f.pool.Exec(ctx, "DELETE FROM environments WHERE pull_request_id=$1", f.pr); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "DELETE FROM pull_requests WHERE id=$1", f.pr); err != nil {
		t.Fatalf("delete the pull request: %v", err)
	}
	for _, table := range []string{"preview_publications", "pr_review_instructions", "preview_feedback_threads", "preview_feedback_messages", "pr_review_counters", "activity_events"} {
		var n int
		if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE pull_request_id=$1", f.pr).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d %v", table, n, err)
		}
	}
}

func TestReviewChangesWakeTheRepositoryPage(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN previa_events"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateFinding(ctx, f.as(f.reviewer), FindingInput{Body: "Wake up", RequestKey: key(1)}); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		n, err := conn.Conn().WaitForNotification(wait)
		if err != nil {
			t.Fatal("no change signal for the repository:", err)
		}
		if strings.Contains(n.Payload, `"repository:`) && strings.Contains(n.Payload, "preview_feedback") {
			return
		}
	}
}
