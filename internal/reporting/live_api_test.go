package reporting

import (
	"context"
	"errors"
	"strings"
)

// liveProofAPI isolates test feedback from a preview's actual living comment.
// Production rendering and discovery keep their usual marker; only this
// test adapter translates it at the GitHub boundary.
type liveProofAPI struct {
	GitHub
	marker string
}

func (a liveProofAPI) FindComment(ctx context.Context, installationID int64, repo Repo, number int32, _ string, maxPages int) (Comment, bool, error) {
	return a.GitHub.FindComment(ctx, installationID, repo, number, a.marker, maxPages)
}

func (a liveProofAPI) CreateComment(ctx context.Context, installationID int64, repo Repo, number int32, body string) (int64, error) {
	body, err := a.body(body)
	if err != nil {
		return 0, err
	}
	return a.GitHub.CreateComment(ctx, installationID, repo, number, body)
}

func (a liveProofAPI) UpdateComment(ctx context.Context, installationID int64, repo Repo, id int64, body string) error {
	comment, err := a.GetComment(ctx, installationID, repo, id)
	if err != nil {
		return err
	}
	if !comment.AppOwned || !strings.HasPrefix(comment.Body, a.marker+"\n") {
		return errors.New("live proof refused to update a comment outside its scope")
	}
	body, err = a.body(body)
	if err != nil {
		return err
	}
	return a.GitHub.UpdateComment(ctx, installationID, repo, id, body)
}

func (a liveProofAPI) body(body string) (string, error) {
	content, ok := strings.CutPrefix(body, Marker+"\n")
	if !ok || a.marker == "" || strings.Contains(a.marker, Marker) {
		return "", errors.New("live proof comment has an invalid marker")
	}
	return a.marker + "\n" + content, nil
}
