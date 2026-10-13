package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-github/v91/github"
)

func TestFakeGitHubSeparatesPullRequestComments(t *testing.T) {
	f := newFakeGitHub(t)
	f.push(e2eCommit1, e2eApp("base"))
	f.pushFork(8, e2eCommit2, e2eApp("fork"))
	client, err := f.InstallationClient(900)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	create := func(number int, body string) int64 {
		t.Helper()
		comment, _, err := client.Issues.CreateComment(ctx, e2eOwner, e2eRepo, number, github.IssueCommentRequest{Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return comment.GetID()
	}
	base := create(e2ePRNumber, "base preview")
	fork := create(8, "fork preview")
	assertComment := func(number int, id int64, body string) {
		t.Helper()
		comments, _, err := client.Issues.ListComments(ctx, e2eOwner, e2eRepo, number, nil)
		if err != nil || len(comments) != 1 {
			t.Fatalf("PR %d comments: %v, %v", number, comments, err)
		}
		if comments[0].GetID() != id || comments[0].GetBody() != body {
			t.Fatalf("PR %d comment: %+v", number, comments[0])
		}
	}
	assertComment(e2ePRNumber, base, "base preview")
	assertComment(8, fork, "fork preview")
	if _, _, err := client.Issues.UpdateComment(ctx, e2eOwner, e2eRepo, fork, github.IssueCommentRequest{Body: "fork updated"}); err != nil {
		t.Fatal(err)
	}
	assertComment(8, fork, "fork updated")
	if _, err := client.Issues.DeleteComment(ctx, e2eOwner, e2eRepo, fork); err != nil {
		t.Fatal(err)
	}
	assertComment(e2ePRNumber, base, "base preview")
	if f.livingComment() != "base preview" || f.commentCount() != 1 {
		t.Fatal("fork operations changed the base PR comment")
	}
	res, err := f.server.Client().Get(fmt.Sprintf("%s/repos/%s/%s/issues/9/comments", f.server.URL, e2eOwner, e2eRepo))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown PR: HTTP %d", res.StatusCode)
	}
}
