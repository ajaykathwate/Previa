package onboarding

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/onboarding/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	pages "github.com/ajaykathwatee/previa/web/pages/onboarding"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	// pickerSize bounds the pull requests offered in the picker; the pull
	// request page links to any other.
	pickerSize = 20
	// recheckAfter keeps a double click or a reload from asking twice.
	recheckAfter = 20 * time.Second
	// DefaultAllowance is how many checks a workspace may request an hour.
	DefaultAllowance = 60
)

var (
	// ErrUnavailable: the repository or pull request is not there for this person.
	ErrUnavailable = errors.New("repository setup is unavailable")
	// ErrClosed: the pull request is no longer open.
	ErrClosed = errors.New("the pull request is closed")
	// ErrChanged: the pull request has a newer head than the page showed.
	ErrChanged = errors.New("the pull request changed")
	// ErrAccess: GitHub access to the repository is unavailable.
	ErrAccess = errors.New("GitHub access to the repository is unavailable")
	// ErrNoGitHub: the server cannot read GitHub (no App configured).
	ErrNoGitHub = errors.New("this server has no GitHub App")
	// ErrNotDeployed: the pull request has no preview to open.
	ErrNotDeployed = errors.New("no preview is deployed")
)

// LimitError says the workspace used its hourly setup checks.
type LimitError struct{ ResetsAt time.Time }

func (e *LimitError) Error() string { return "setup check allowance used" }

// CheckOutcome is what a check request did.
type CheckOutcome string

const (
	CheckRequested CheckOutcome = "requested" // a worker will check the commit
	CheckRunning   CheckOutcome = "running"   // a check of this commit is already on its way
	CheckRecent    CheckOutcome = "recent"    // it was asked for moments ago
)

// Capabilities says what the running workers can do; *workers.Reader
// implements it.
type Capabilities interface {
	Current(ctx context.Context) (workers.Capabilities, error)
}

// Secrets lists a repository's runtime secret names for people who manage
// them; the secrets feature implements it and enforces who may.
type Secrets interface {
	Names(ctx context.Context, userID, repoID int64) ([]string, error)
	Configured() bool
}

type jobInserter interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Options are the server settings the service needs.
type Options struct {
	PreviewDomain string
	Production    bool // hosted wording: no operator commands on customer pages
	Discover      bool // the server has the GitHub App
	Allowance     int  // checks per workspace and hour; zero uses DefaultAllowance
}

// Service decides what the Setup tab shows and records check requests and
// preview opens.
type Service struct {
	pool     *pgxpool.Pool
	jobs     jobInserter
	platform PlatformSource
	caps     Capabilities
	secrets  Secrets
	opts     Options
	now      func() time.Time
}

// NewService composes the state the checklist is derived from.
func NewService(pool *pgxpool.Pool, jobs jobInserter, platform PlatformSource, caps Capabilities, secrets Secrets, opts Options) (*Service, error) {
	if pool == nil || jobs == nil || platform == nil || caps == nil || secrets == nil {
		return nil, errors.New("repository setup needs a database, a queue, a platform, worker capabilities and secrets")
	}
	if opts.Allowance <= 0 {
		opts.Allowance = DefaultAllowance
	}
	return &Service{pool: pool, jobs: jobs, platform: platform, caps: caps, secrets: secrets, opts: opts, now: time.Now}, nil
}

// Page loads a repository's setup checklist for one open pull request: the
// requested one, else where setup last continued, else the newest.
func (s *Service) Page(ctx context.Context, userID, repoID int64, number int32) (pages.Page, error) {
	q := dbgen.New(s.pool)
	repo, err := q.SetupRepository(ctx, dbgen.SetupRepositoryParams{RepoID: repoID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pages.Page{}, ErrUnavailable
	}
	if err != nil {
		return pages.Page{}, fmt.Errorf("load repository for setup: %w", err)
	}
	p := pages.Page{RepoID: repo.ID, Name: repo.FullName, Mode: repo.BuildMode}
	if p.OpenCount, err = q.CountOpenPullRequests(ctx, repo.ID); err != nil {
		return pages.Page{}, fmt.Errorf("count open pull requests: %w", err)
	}
	options, err := q.OpenPullRequests(ctx, dbgen.OpenPullRequestsParams{RepoID: repo.ID, RowLimit: pickerSize})
	if err != nil {
		return pages.Page{}, fmt.Errorf("list open pull requests: %w", err)
	}
	for _, o := range options {
		p.Options = append(p.Options, pages.Option{Number: o.Number, Title: o.Title, Branch: o.HeadBranch})
	}
	v := view{now: s.now(), viewer: userID, repo: repo, discover: s.opts.Discover, production: s.opts.Production, previewDomain: s.opts.PreviewDomain}
	if v.caps, err = s.caps.Current(ctx); err != nil {
		return pages.Page{}, fmt.Errorf("read worker capabilities: %w", err)
	}
	if _, v.fingerprint, err = s.platform.Platform(ctx); err != nil {
		return pages.Page{}, err
	}
	selected := number
	if selected == 0 {
		latest, err := q.LatestSetupPullRequest(ctx, repo.ID)
		switch {
		case err == nil:
			selected = latest
		case !errors.Is(err, pgx.ErrNoRows):
			return pages.Page{}, fmt.Errorf("find the pull request setup continues on: %w", err)
		case len(options) > 0:
			selected = options[0].Number
		}
	}
	if selected > 0 {
		row, err := q.SetupPullRequest(ctx, dbgen.SetupPullRequestParams{RepoID: repo.ID, Number: selected})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return pages.Page{}, ErrUnavailable
		case err != nil:
			return pages.Page{}, fmt.Errorf("load pull request setup: %w", err)
		case row.State != environments.StateOpen:
			return pages.Page{}, ErrClosed
		}
		v.pr = &row
		if !p.Offers(row.Number) {
			p.Options = append(p.Options, pages.Option{Number: row.Number, Title: row.Title, Branch: row.HeadBranch})
		}
		if v.live() {
			opened, err := q.LatestOpen(ctx, dbgen.LatestOpenParams{PullRequestID: row.ID, DeployedSha: row.DeployedSha, ViewerID: userID})
			switch {
			case err == nil:
				v.opened = &opened
			case !errors.Is(err, pgx.ErrNoRows):
				return pages.Page{}, fmt.Errorf("read preview opens: %w", err)
			}
		}
	} else if p.Sync, err = s.syncState(ctx, q, repo.ID); err != nil {
		return pages.Page{}, err
	}
	v.secretsOn = s.secrets.Configured()
	if repo.CanManage {
		if v.secrets, err = s.secrets.Names(ctx, userID, repo.ID); err != nil {
			return pages.Page{}, fmt.Errorf("list secret names: %w", err)
		}
	} else if v.managers, err = s.managers(ctx, q, repo.WorkspaceID); err != nil {
		return pages.Page{}, err
	}
	v.derive(&p)
	return p, nil
}

func (s *Service) syncState(ctx context.Context, q *dbgen.Queries, repoID int64) (pages.Sync, error) {
	row, err := q.SyncState(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pages.Sync{}, nil
	}
	if err != nil {
		return pages.Sync{}, fmt.Errorf("read pull request sync: %w", err)
	}
	running := (row.State == "queued" || row.State == "syncing") && row.RequestedAt.Valid && s.now().Sub(row.RequestedAt.Time) < 5*time.Minute
	return pages.Sync{Running: running, Last: stamp(row.LastSuccessAt)}, nil
}

func (s *Service) managers(ctx context.Context, q *dbgen.Queries, workspaceID int64) ([]pages.Person, error) {
	rows, err := q.WorkspaceManagers(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace managers: %w", err)
	}
	people := make([]pages.Person, 0, len(rows))
	for _, r := range rows {
		people = append(people, pages.Person{Login: r.Username, Name: r.DisplayName, Role: r.Role})
	}
	return people, nil
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// RequestCheck asks a worker to check a pull request's head commit, the one
// the page showed. A check already on its way, or asked for moments ago, is
// not asked for twice; a workspace has an hourly allowance.
func (s *Service) RequestCheck(ctx context.Context, userID, repoID int64, number int32, expectedSHA string) (outcome CheckOutcome, err error) {
	if !s.opts.Discover {
		return "", ErrNoGitHub
	}
	err = database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		pr, err := q.CheckTarget(ctx, dbgen.CheckTargetParams{RepoID: repoID, Number: number, UserID: userID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrUnavailable
		case err != nil:
			return fmt.Errorf("load the pull request to check: %w", err)
		case pr.State != environments.StateOpen:
			return ErrClosed
		case !pr.Accessible || pr.InstallationStatus != "active":
			return ErrAccess
		case pr.HeadSha != expectedSHA:
			return ErrChanged
		case !fullSHA.MatchString(pr.HeadSha):
			return ErrUnavailable
		}
		current, err := q.LockCheck(ctx, pr.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock setup check: %w", err)
		}
		if err == nil && current.HeadSha == pr.HeadSha {
			age := s.now().Sub(current.RequestedAt.Time)
			switch {
			case (current.State == "queued" || current.State == "running") && age < CheckStaleAfter:
				outcome = CheckRunning
				return nil
			case age < recheckAfter:
				outcome = CheckRecent
				return nil
			}
		}
		if _, err := q.SpendCheckBudget(ctx, dbgen.SpendCheckBudgetParams{WorkspaceID: pr.WorkspaceID, Allowance: int32(s.opts.Allowance)}); errors.Is(err, pgx.ErrNoRows) {
			reset, err := q.CheckBudgetReset(ctx, pr.WorkspaceID)
			if err != nil {
				return fmt.Errorf("read setup check allowance: %w", err)
			}
			return &LimitError{ResetsAt: reset.Time}
		} else if err != nil {
			return fmt.Errorf("spend setup check allowance: %w", err)
		}
		request, err := q.RequestCheck(ctx, dbgen.RequestCheckParams{PullRequestID: pr.ID, HeadSha: pr.HeadSha, Generation: pr.Generation, RequestedBy: pgtype.Int8{Int64: userID, Valid: true}})
		if err != nil {
			return fmt.Errorf("record setup check request: %w", err)
		}
		if _, err := s.jobs.InsertTx(ctx, tx, CheckArgs{CheckID: request.ID, Revision: request.Revision}, nil); err != nil {
			return fmt.Errorf("queue setup check: %w", err)
		}
		outcome = CheckRequested
		return nil
	})
	return outcome, err
}

// Open returns the address of a pull request's deployed preview and, when the
// person asked from Previa itself, records that they opened it.
func (s *Service) Open(ctx context.Context, userID, repoID int64, number int32, record bool) (string, error) {
	row, err := dbgen.New(s.pool).OpenTarget(ctx, dbgen.OpenTargetParams{RepoID: repoID, Number: number, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnavailable
	}
	if err != nil {
		return "", fmt.Errorf("load the preview to open: %w", err)
	}
	deployed := row.DeployedSha != "" && (row.EnvironmentStatus == environments.StatusRunning || row.EnvironmentStatus == environments.StatusFailed || row.EnvironmentStatus == "publishing")
	if !deployed || row.Hostname == "" || s.opts.PreviewDomain == "" {
		return "", ErrNotDeployed
	}
	if record {
		if err := dbgen.New(s.pool).RecordOpen(ctx, dbgen.RecordOpenParams{PullRequestID: row.ID, UserID: userID, DeployedSha: row.DeployedSha}); err != nil {
			return "", fmt.Errorf("record preview open: %w", err)
		}
	}
	return "https://" + row.Hostname + "." + s.opts.PreviewDomain, nil
}

// Journey is how far a workspace's repositories this person can build in
// have come, for the Overview's next steps.
type Journey struct {
	Repositories, Available    int64
	Checked, Published, Opened bool
	FocusID                    int64 // where setup continues; 0 when none is available
	FocusName                  string
	Managers                   []pages.Person
}

// Journey reads the Overview's onboarding state for a workspace.
func (s *Service) Journey(ctx context.Context, workspaceID, userID int64) (Journey, error) {
	if workspaceID <= 0 || userID <= 0 {
		return Journey{}, nil
	}
	q := dbgen.New(s.pool)
	row, err := q.Journey(ctx, dbgen.JourneyParams{WorkspaceID: workspaceID, UserID: userID})
	if err != nil {
		return Journey{}, fmt.Errorf("read onboarding progress: %w", err)
	}
	j := Journey{Repositories: row.Repositories, Available: row.Available, Checked: row.Checked || row.Published, Published: row.Published, Opened: row.Opened}
	focus, err := q.FocusRepository(ctx, dbgen.FocusRepositoryParams{WorkspaceID: workspaceID, UserID: userID})
	switch {
	case err == nil:
		j.FocusID, j.FocusName = focus.ID, focus.FullName
	case !errors.Is(err, pgx.ErrNoRows):
		return Journey{}, fmt.Errorf("find where setup continues: %w", err)
	}
	if j.Repositories == 0 {
		if j.Managers, err = s.managers(ctx, q, workspaceID); err != nil {
			return Journey{}, err
		}
	}
	return j, nil
}
