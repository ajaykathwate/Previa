package onboarding

import (
	"context"
	"errors"
	"path"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/ajaykathwatee/previa/internal/source"
)

// Entry kinds a Tree reports for a path.
const (
	EntryFile      = "file"
	EntryDir       = "dir"
	EntryLink      = "symlink"
	EntrySubmodule = "submodule"
	EntryMissing   = "missing"
	EntryLarge     = "large" // it exists, but GitHub's answer was too large to inspect
)

// Tree reads one repository at one exact commit. GitHubFiles implements it
// with bounded GitHub API reads; tests use a map.
type Tree interface {
	// Config returns .previewrc.yaml at the commit: ErrMissing when the
	// commit is readable but has none, ErrNotRegular for a directory and
	// ErrTooLarge above repoconfig.MaxConfigBytes.
	Config(ctx context.Context) ([]byte, error)
	// Stat reports what a path is, EntryMissing included.
	Stat(ctx context.Context, name string) (string, error)
	// Read returns a small regular file, or ErrMissing.
	Read(ctx context.Context, name string, limit int64) ([]byte, error)
}

// Sentinels a Tree returns for a commit that answers normally.
var (
	ErrMissing    = errors.New("no such file at the commit")
	ErrNotRegular = errors.New("not a regular file")
	ErrTooLarge   = errors.New("file too large")
)

// Why an attempt ended without a result, stored as failure_code. A missing
// or invalid configuration is a result, not an attempt failure.
const (
	FailRateLimited  = "rate_limited"       // GitHub asked Previa to slow down
	FailGitHubDown   = "github_unavailable" // GitHub could not be reached or failed
	FailGitHubError  = "github_error"       // GitHub answered something unusable
	FailAccess       = "access_unavailable" // the App can no longer read this repository
	FailApp          = "app_unavailable"    // no GitHub App credentials on this worker
	FailStaleCommit  = "stale_commit"       // a newer commit arrived while checking
	FailClosed       = "pull_request_closed"
	FailInterrupted  = "interrupted" // the worker stopped before it finished
	FailInternal     = "internal"
	FailNotSupported = "not_supported" // the repository has no GitHub ID recorded
)

// AttemptError is a classified reason an attempt could not finish. It never
// carries GitHub response bodies, tokens or URLs.
type AttemptError struct{ Code string }

func (e *AttemptError) Error() string { return "setup check: " + e.Code }

// failureCode classifies an attempt's error for storage.
func failureCode(err error) string {
	var attempt *AttemptError
	if errors.As(err, &attempt) {
		return attempt.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FailGitHubDown
	}
	return FailInternal
}

// Check runs the static setup checks for one commit: the configuration a build
// would read, parsed by the build's own parser against what this server offers,
// and the build context, Dockerfile and submodule manifest it relies on.
// Runtime facts (readiness, credentials, migrations) are left to the build.
func Check(ctx context.Context, tree Tree, platform repoconfig.Platform) (Result, error) {
	data, err := tree.Config(ctx)
	switch {
	case errors.Is(err, ErrMissing):
		r := Result{Config: ConfigMissing}
		// Only to suggest the right Starter kit; a failure here changes nothing.
		if kind, err := tree.Stat(ctx, "Dockerfile"); err == nil && kind == EntryFile {
			r.RootDockerfile = true
		}
		return r.finish(), nil
	case errors.Is(err, ErrNotRegular):
		return invalidConfig([]repoconfig.Issue{{Field: "$", Code: "invalid_file", Message: repoconfig.Filename + " must be a regular file at the repository root, not a directory or link."}}), nil
	case errors.Is(err, ErrTooLarge):
		return invalidConfig([]repoconfig.Issue{{Field: "$", Code: "too_large", Message: "Configuration must not exceed 64 KiB."}}), nil
	case err != nil:
		return Result{}, err
	}
	defer clear(data)
	parsed, err := repoconfig.ParseFor(data, platform)
	if err != nil {
		var invalid *repoconfig.ValidationError
		if errors.As(err, &invalid) {
			return invalidConfig(invalid.Issues), nil
		}
		return Result{}, &AttemptError{Code: FailInternal}
	}
	r := fromConfig(parsed)
	if r.ContextState, r.DockerfileState, err = probeDockerfile(ctx, tree, r.Context, r.Dockerfile); err != nil {
		return Result{}, err
	}
	if r.Submodules, err = submodulesBlock(ctx, tree, r.Context); err != nil {
		return Result{}, err
	}
	return r.finish(), nil
}

// probeDockerfile finds the Dockerfile inside the build context. A missing
// file in a subfolder context asks once more whether the context exists.
func probeDockerfile(ctx context.Context, tree Tree, contextDir, dockerfile string) (contextState, dockerfileState string, err error) {
	kind, err := tree.Stat(ctx, path.Join(contextDir, dockerfile))
	if err != nil {
		return "", "", err
	}
	switch kind {
	case EntryFile, EntryLarge:
		return PathFound, PathFound, nil
	case EntryLink:
		// The build follows links that stay inside the tree; GitHub's API
		// cannot tell where one leads.
		return PathFound, PathUnverified, nil
	case EntryDir, EntrySubmodule:
		return PathFound, PathNotFile, nil
	}
	if contextDir == "." {
		return PathFound, PathMissing, nil
	}
	switch kind, err = tree.Stat(ctx, contextDir); {
	case err != nil:
		return "", "", err
	case kind == EntryDir:
		return PathFound, PathMissing, nil
	case kind == EntryMissing:
		return PathMissing, PathUnverified, nil
	case kind == EntryLink || kind == EntryLarge:
		return PathUnverified, PathUnverified, nil
	default:
		return PathNotDir, PathUnverified, nil
	}
}

// submodulesBlock applies the build's own submodule rule to the root
// manifest. Manifests nested inside the context are found by the build.
func submodulesBlock(ctx context.Context, tree Tree, contextDir string) (bool, error) {
	manifest, err := tree.Read(ctx, ".gitmodules", 64<<10)
	switch {
	case errors.Is(err, ErrMissing):
		return false, nil
	case errors.Is(err, ErrNotRegular), errors.Is(err, ErrTooLarge):
		return true, nil
	case err != nil:
		return false, err
	}
	return source.SubmodulesInContext(manifest, contextDir), nil
}
