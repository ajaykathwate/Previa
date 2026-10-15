package pullrequests_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pr "github.com/ajaykathwatee/previa/internal/pullrequests"
)

// commitFake answers head-commit reads and counts them; a zero value fails.
type commitFake struct {
	mu      sync.Mutex
	calls   int
	details *pr.CommitDetails
}

func (c *commitFake) Commit(_ context.Context, installationID, repoID int64, sha string) (pr.CommitDetails, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.details == nil || installationID <= 0 || repoID <= 0 || sha == "" {
		return pr.CommitDetails{}, errors.New("GitHub unavailable")
	}
	return *c.details, nil
}

func (c *commitFake) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }

func TestHeadCommitMessageIsFetchedOncePerHeadAndEscaped(t *testing.T) {
	h := newActionsHarness(t)
	h.commits.details = &pr.CommitDetails{Message: "Fix login <b>redirect</b>\n\nKeeps the next page after sign-in.", Author: "Ada Committer", At: time.Now().Add(-time.Hour)}
	html := h.request(t, "GET", detailPath, "").Body.String()
	for _, want := range []string{"Fix login &lt;b&gt;redirect&lt;/b&gt;", "Full commit message", "Keeps the next page after sign-in.", "Ada Committer"} {
		if !strings.Contains(html, want) {
			t.Fatalf("commit details missing %q", want)
		}
	}
	h.request(t, "GET", detailPath, "")
	if h.commits.count() != 1 {
		t.Fatalf("cached head commit fetched again: %d calls", h.commits.count())
	}
	h.exec(t, "UPDATE pull_requests SET head_sha='sha-new' WHERE id=$1", h.prID)
	h.commits.details = nil
	if html := h.request(t, "GET", detailPath, "").Body.String(); strings.Contains(html, "commit-subject") {
		t.Fatal("an older commit's message was shown for a new head")
	}
	h.request(t, "GET", detailPath, "")
	if h.commits.count() != 2 {
		t.Fatalf("a failed fetch must back off before retrying: %d calls", h.commits.count())
	}
	h.exec(t, "UPDATE pull_requests SET head_commit_checked_at=now()-interval '11 minutes' WHERE id=$1", h.prID)
	h.commits.details = &pr.CommitDetails{Message: "Second try"}
	if html := h.request(t, "GET", detailPath, "").Body.String(); !strings.Contains(html, "Second try") || h.commits.count() != 3 {
		t.Fatal("failed head commit fetch was not retried after the back-off")
	}
}
