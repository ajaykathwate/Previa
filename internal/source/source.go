package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"time"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

// Request identifies one exact commit of a tracked PR. The archive always comes
// from the base repository, resolved by its immutable numeric ID.
type Request struct {
	InstallationID int64  // GitHub installation that can read the base repository
	BaseRepoID     int64  // GitHub repository ID of the PR's base
	CommitSHA      string // full 40-character lowercase head commit SHA
	// Local identities, used to name the workspace and attribute leftovers.
	PullRequestID int64
	Generation    int64
	Revision      int64
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (r Request) validate() error {
	if r.InstallationID <= 0 || r.BaseRepoID <= 0 || r.PullRequestID <= 0 || r.Generation < 0 || r.Revision < 0 || !fullSHA.MatchString(r.CommitSHA) {
		return errors.New("source preparation needs an installation, base repository, PR identity and full lowercase 40-character commit SHA")
	}
	return nil
}

// ArchiveSource opens the gzip tarball for one exact commit. GitHubArchives is
// the production implementation.
type ArchiveSource interface {
	Open(ctx context.Context, request Request) (io.ReadCloser, int64, error)
}

// Preparer turns a verified commit into a workspace ready for an image build.
type Preparer struct {
	archives   ArchiveSource
	workspaces *Workspaces
	limits     Limits
	resources  repoconfig.Resources
	now        func() time.Time
}

func NewPreparer(archives ArchiveSource, workspaces *Workspaces, limits Limits, resources repoconfig.Resources) (*Preparer, error) {
	if archives == nil || workspaces == nil {
		return nil, errors.New("source preparation requires an archive source and workspaces")
	}
	if resources.CPUMillis <= 0 || resources.MemoryBytes <= 0 {
		return nil, errors.New("source preparation requires positive platform resource ceilings")
	}
	return &Preparer{archives: archives, workspaces: workspaces, limits: limits.withDefaults(), resources: resources, now: time.Now}, nil
}

// Prepared is an extracted, identity-verified source tree and its configuration.
type Prepared struct {
	Workspace  WorkspaceInfo
	ContextDir string            // absolute build context: the extracted tree
	Dockerfile string            // relative to ContextDir; resolves to a regular file inside it
	Config     repoconfig.Result // parsed from the same tree; Config.Env may hold values, never log it
	Stats      Stats
}

// Prepare downloads, verifies and extracts the commit into a fresh workspace and
// loads .previewrc.yaml from it. On any failure or cancellation the workspace is
// removed; if removal itself fails, Workspaces.List still finds it for cleanup.
// Release the returned workspace once its build finishes or fails.
func (p *Preparer) Prepare(ctx context.Context, request Request) (prepared *Prepared, err error) {
	if err := request.validate(); err != nil {
		return nil, failure(CodeInvalidRequest, "The source request is incomplete or not an exact commit.", err)
	}
	ctx, cancel := context.WithTimeout(ctx, p.limits.Timeout)
	defer cancel()
	workspace, err := p.workspaces.create(request, p.now())
	if err != nil {
		if workspace.Path != "" {
			_ = p.workspaces.Remove(workspace)
		}
		return nil, retryable(CodeWorkspaceFailure, "The worker could not create a source workspace.", err)
	}
	defer func() {
		if err != nil {
			_ = p.workspaces.Remove(workspace) // Leftovers stay discoverable through List.
		}
	}()
	body, size, err := p.archives.Open(ctx, request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	if size > p.limits.MaxArchiveBytes {
		return nil, failure(CodeTooLarge, "The source archive exceeds the download size limit.", nil)
	}
	root, err := os.OpenRoot(workspace.SourceDir())
	if err != nil {
		return nil, retryable(CodeWorkspaceFailure, "The worker could not open the source workspace.", err)
	}
	defer func() { _ = root.Close() }()
	stats, err := extract(ctx, body, root, request.CommitSHA, p.limits)
	if err != nil {
		return nil, err
	}
	result, err := loadConfig(root, p.resources)
	if err != nil {
		return nil, err
	}
	if err := checkDockerfile(root, result.Config.Dockerfile); err != nil {
		return nil, err
	}
	return &Prepared{Workspace: workspace, ContextDir: workspace.SourceDir(), Dockerfile: result.Config.Dockerfile, Config: result, Stats: stats}, nil
}

// Release removes a prepared workspace.
func (p *Preparer) Release(prepared *Prepared) error {
	if prepared == nil {
		return nil
	}
	return p.workspaces.Remove(prepared.Workspace)
}

// loadConfig reads .previewrc.yaml from the extracted tree with the same parser,
// defaults, clamping and hash as repoconfig. The file must be a regular file at
// the root; it is opened without following a link swapped in after the check.
func loadConfig(root *os.Root, resources repoconfig.Resources) (repoconfig.Result, error) {
	info, err := root.Lstat(repoconfig.Filename)
	if errors.Is(err, fs.ErrNotExist) {
		return repoconfig.Result{}, failure(CodeMissingConfig, "Add .previewrc.yaml to the repository root at the requested commit.", nil)
	}
	if err != nil {
		return repoconfig.Result{}, retryable(CodeWorkspaceFailure, "The worker could not read the source tree.", err)
	}
	if !info.Mode().IsRegular() {
		return repoconfig.Result{}, failure(CodeInvalidConfig, ".previewrc.yaml must be a regular file, not a link or directory.", nil)
	}
	if info.Size() > repoconfig.MaxConfigBytes {
		return repoconfig.Result{}, failure(CodeInvalidConfig, "Configuration must not exceed 64 KiB.", nil)
	}
	f, err := root.OpenFile(repoconfig.Filename, os.O_RDONLY|syscallNoFollow, 0)
	if err != nil {
		return repoconfig.Result{}, failure(CodeInvalidConfig, ".previewrc.yaml must be a regular file, not a link or directory.", nil)
	}
	data, err := io.ReadAll(io.LimitReader(f, repoconfig.MaxConfigBytes+1))
	_ = f.Close()
	defer clear(data)
	if err != nil {
		return repoconfig.Result{}, retryable(CodeWorkspaceFailure, "The worker could not read the source tree.", err)
	}
	if len(data) > repoconfig.MaxConfigBytes {
		return repoconfig.Result{}, failure(CodeInvalidConfig, "Configuration must not exceed 64 KiB.", nil)
	}
	result, err := repoconfig.Parse(data, resources)
	if err != nil {
		var invalid *repoconfig.ValidationError
		if errors.As(err, &invalid) {
			return repoconfig.Result{}, failure(CodeInvalidConfig, invalid.Error(), err)
		}
		return repoconfig.Result{}, failure(CodeInvalidConfig, "The preview configuration is invalid.", err)
	}
	return result, nil
}

// checkDockerfile resolves the configured path through os.Root, which follows
// only links that stay inside the tree, and requires a regular file.
func checkDockerfile(root *os.Root, dockerfile string) error {
	info, err := root.Stat(dockerfile)
	if errors.Is(err, fs.ErrNotExist) {
		return failure(CodeMissingDockerfile, fmt.Sprintf("The Dockerfile %q named in .previewrc.yaml does not exist at the requested commit.", dockerfile), nil)
	}
	if err != nil || !info.Mode().IsRegular() {
		return failure(CodeMissingDockerfile, "The Dockerfile must be a regular file inside the repository.", nil)
	}
	return nil
}
