package source

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/githubapp"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

// TestRealSourcePrepare is an opt-in check against real GitHub (make test-source).
// It reads one PR through the installation, prepares its head commit with the
// production archive source, and logs exactly what was verified. It never prints
// tokens or archive URLs and writes nothing to GitHub.
func TestRealSourcePrepare(t *testing.T) {
	installation, repoID, number := envInt(t, "TEST_GITHUB_INSTALLATION_ID"), envInt(t, "TEST_GITHUB_REPO_ID"), envInt(t, "TEST_GITHUB_PR_NUMBER")
	if installation == 0 || repoID == 0 || number == 0 {
		t.Skip("set TEST_GITHUB_INSTALLATION_ID, TEST_GITHUB_REPO_ID and TEST_GITHUB_PR_NUMBER (make test-source) to run against real GitHub")
	}
	app, err := githubapp.NewApp(envInt(t, "GITHUB_APP_ID"), os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	client, err := app.InstallationClient(installation)
	if err != nil {
		t.Fatal(err)
	}
	repo, _, err := client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		t.Fatalf("read base repository: %v", err)
	}
	pr, _, err := client.PullRequests.Get(ctx, repo.GetOwner().GetLogin(), repo.GetName(), int(number))
	if err != nil {
		t.Fatalf("read pull request: %v", err)
	}
	sha, headRepo := pr.GetHead().GetSHA(), pr.GetHead().GetRepo().GetID()
	t.Logf("PR #%d state=%s head=%s head_repo=%d base_repo=%d fork=%v", pr.GetNumber(), pr.GetState(), sha, headRepo, repoID, headRepo == 0 || headRepo != repoID)

	archives, err := NewGitHubArchives(app)
	if err != nil {
		t.Fatal(err)
	}
	workspaces, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	preparer, err := NewPreparer(archives, workspaces, Limits{}, repoconfig.Platform{Limits: repoconfig.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := preparer.Prepare(ctx, Request{InstallationID: installation, BaseRepoID: repoID, CommitSHA: sha, PullRequestID: 1, Generation: 1, Revision: 1})
	switch CodeOf(err) {
	case "":
		t.Logf("verified: archive declared commit %s; extracted %d files, %d dirs, %d symlinks, %d bytes from %d compressed bytes", sha, prepared.Stats.Files, prepared.Stats.Directories, prepared.Stats.Symlinks, prepared.Stats.Bytes, prepared.Stats.ArchiveBytes)
		t.Logf("verified: .previewrc.yaml parsed (hash %s…), Dockerfile %q is a regular file in the tree", prepared.Config.Hash[:12], prepared.Dockerfile)
		if err := preparer.Release(prepared); err != nil {
			t.Fatal(err)
		}
	case CodeMissingConfig, CodeInvalidConfig, CodeMissingDockerfile:
		// These are reported only after the archive was downloaded, its commit
		// identity verified and the tree extracted safely.
		t.Logf("verified: archive declared commit %s and extracted safely; configuration outcome: %s (%v)", sha, CodeOf(err), err)
	default:
		t.Fatalf("real source preparation failed: %s: %v", CodeOf(err), err)
	}
	if left, err := workspaces.List(); err != nil || len(left) != 0 {
		t.Fatalf("workspace not cleaned up: %v %v", left, err)
	}
}

func envInt(t *testing.T, name string) int64 {
	value := os.Getenv(name)
	if value == "" {
		return 0
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		t.Fatalf("%s must be a positive integer", name)
	}
	return n
}
