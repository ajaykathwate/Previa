package accounts

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/auth"
	"github.com/ajaykathwatee/previa/internal/workspaces"
)

func TestGoogleReviewerInvitationLinkingAndTechnicalRoles(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	users := auth.NewRepository(f.pool)
	reviewer, err := users.SaveGoogle(ctx, auth.Identity{GoogleSubject: "google-reviewer", Name: "Client Reviewer", Email: "reviewer@example.test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.GitHubID != 0 {
		t.Fatal("Google sign-in invented a GitHub identity")
	}
	team := workspaces.NewService(workspaces.NewRepository(f.pool), nil, nil, nil)
	if _, err = team.Create(ctx, reviewer.ID, "Not allowed"); !errors.Is(err, workspaces.ErrGitHubRequired) {
		t.Fatal(err)
	}
	revision := f.id(t, `SELECT revision FROM workspaces WHERE id=$1`, f.workspace)
	command := workspaces.InvitationCommand{ActorID: f.owner, WorkspaceID: f.workspace, Revision: revision, Login: "Reviewer@Example.Test", Role: "reviewer", Repositories: []int64{f.repo}}
	token, err := team.Invite(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = team.Invite(ctx, command); err == nil {
		t.Fatal("duplicate email invitation accepted")
	}
	command.Login = "developer@example.test"
	command.Role = "developer"
	if _, err = team.Invite(ctx, command); !errors.Is(err, workspaces.ErrEmailInvitation) {
		t.Fatal("email assigned technical role", err)
	}
	wrong, err := users.SaveGoogle(ctx, auth.Identity{GoogleSubject: "other", Email: "stranger@example.test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = team.RespondInvitation(ctx, wrong.ID, 0, token, true); !errors.Is(err, workspaces.ErrInvitation) {
		t.Fatal("forwarded invitation accepted", err)
	}
	received, err := team.ReceivedForUser(ctx, reviewer.ID)
	if err != nil || len(received) != 1 {
		t.Fatal(received, err)
	}
	if _, err = team.InvitationForUser(ctx, reviewer.ID, 0, token); err != nil {
		t.Fatal(err)
	}
	if _, err = team.RespondInvitation(ctx, reviewer.ID, 0, token, true); err != nil {
		t.Fatal(err)
	}
	if _, err = team.RespondInvitation(ctx, reviewer.ID, 0, token, true); err != nil {
		t.Fatal("duplicate accept", err)
	}
	if _, err = team.Authorize(ctx, reviewer.ID, f.repo, "preview", false); err != nil {
		t.Fatal(err)
	}
	if _, err = team.Authorize(ctx, reviewer.ID, f.repo, "operate", true); !errors.Is(err, workspaces.ErrUnavailable) {
		t.Fatal("reviewer operated repository", err)
	}
	change := workspaces.MemberChange{ActorID: f.owner, WorkspaceID: f.workspace, WorkspaceRevision: f.id(t, `SELECT revision FROM workspaces WHERE id=$1`, f.workspace), UserID: reviewer.ID, Revision: f.id(t, `SELECT revision FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2`, f.workspace, reviewer.ID), Role: "developer", Repositories: []int64{f.repo}}
	if err = team.ChangeMember(ctx, change); !errors.Is(err, workspaces.ErrGitHubRequired) {
		t.Fatal("Google-only member promoted", err)
	}
	linked, err := users.LinkGitHub(ctx, auth.Identity{ID: 999, Username: "client-github"}, reviewer.ID)
	if err != nil || linked.ID != reviewer.ID {
		t.Fatal(linked, err)
	}
	if f.text(t, `SELECT role FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2`, f.workspace, reviewer.ID) != "reviewer" {
		t.Fatal("connecting GitHub promoted role")
	}
	if _, err = team.RespondReceivedInvitation(ctx, reviewer.ID, 999, received[0].ID, true); err != nil {
		t.Fatal("linked login lost invitation identity", err)
	}
	if err = team.ChangeMember(ctx, change); err != nil {
		t.Fatal("explicit promotion after GitHub linking", err)
	}
	signed, err := users.UpsertUser(ctx, auth.Identity{ID: 999, Username: "renamed"})
	if err != nil || signed.ID != reviewer.ID {
		t.Fatal("GitHub login lost Google account", err)
	}
	if _, err = users.SaveGoogle(ctx, auth.Identity{GoogleSubject: "google-reviewer", Email: "reviewer@example.test"}, f.owner); err == nil {
		t.Fatal("another account stole Google identity")
	}
	if _, err = users.LinkGitHub(ctx, auth.Identity{ID: 101, Username: "owner"}, wrong.ID); err == nil {
		t.Fatal("another account stole GitHub identity")
	}
}
func TestGoogleDeletionAndRestoreFence(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	users := auth.NewRepository(f.pool)
	identity := auth.Identity{GoogleSubject: "delete-google", Email: "erase@example.test", Name: "Erase Me"}
	u, err := users.SaveGoogle(ctx, identity, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'reviewer')`, f.workspace, u.ID)
	operation, _, err := f.s.StartAccountDeletion(ctx, u.ID, "DELETE")
	if err != nil {
		t.Fatal(err)
	}
	j := f.journal(t)
	if j.Version != 2 {
		t.Fatal("Google restore must use the provider-aware journal")
	}
	var deletion JournalEvent
	for _, e := range j.Events {
		if e.Action == "account" {
			deletion = e
		}
	}
	if deletion.GoogleSubject == nil || deletion.User != nil {
		t.Fatal("Google deletion identity not journaled")
	}
	if err = f.s.AcknowledgeJournal(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Process(ctx, operation.ID); err != nil {
		t.Fatal(err)
	}
	if f.text(t, `SELECT google_email||email||first_name FROM users WHERE id=$1`, u.ID) != "" {
		t.Fatal("personal Google profile survived erasure")
	}
	if _, err = users.SaveGoogle(ctx, identity, 0); err == nil {
		t.Fatal("deleted Google identity resurrected")
	}
	restored := newFixture(t)
	if err = restored.s.replayEvent(ctx, deletion); err != nil {
		t.Fatal("restore absent Google tombstone", err)
	}
	if _, err = auth.NewRepository(restored.pool).SaveGoogle(ctx, identity, 0); err == nil {
		t.Fatal("post-backup Google identity resurrected")
	}
}
func TestGoogleMigrationRoundTripAndSafeRollbackRefusal(t *testing.T) {
	f := newFixture(t)
	b, err := os.ReadFile("../../db/migrations/0046_google_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("down missing")
	}
	if _, err = f.pool.Exec(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	if _, err = auth.NewRepository(f.pool).SaveGoogle(t.Context(), auth.Identity{GoogleSubject: "rollback", Email: "rollback@example.test"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), down); err == nil {
		t.Fatal("rollback discarded Google identities")
	}
}
