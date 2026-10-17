package sampledata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/sampledata/dbgen"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrUnavailable hides a repository from someone who may not build in it.
	ErrUnavailable = errors.New("test data is unavailable")
	// ErrForbidden: only Owners and Admins change settings or delete baselines.
	ErrForbidden = errors.New("only Owners and Admins change Test data")
	// ErrConflict: the settings changed since the form was shown.
	ErrConflict = errors.New("test data settings changed meanwhile")
	// ErrNoBaseline: Refresh baseline needs Test data with a baseline.
	ErrNoBaseline = errors.New("the repository does not use a baseline")
	// ErrAccessRemoved: Previa lost GitHub access to the repository.
	ErrAccessRemoved = errors.New("GitHub access to the repository was removed")
)

// InvalidError is a settings field to correct.
type InvalidError struct{ Field, Message string }

func (e *InvalidError) Error() string { return e.Field + ": " + e.Message }

// Catalog says what the execution workers offer for test data; internal/app
// adapts the workers' capabilities.
type Catalog interface {
	TestData(ctx context.Context) (Offer, error)
}

// Offer is what the execution workers can do for test data now.
type Offer struct {
	Databases bool // they offer PostgreSQL or MySQL
	Baselines bool // one stores baselines
}

// Scope is a repository as the Test data tab sees it.
type Scope struct {
	RepoID               int64
	FullName             string
	Available, CanManage bool
}

// Refresh is one baseline refresh, for the tab.
type Refresh struct {
	Sequence                                  int64
	Trigger, State                            string
	Branch, CommitSHA                         string
	FailureCode, FailureDetail, WaitingReason string
	Attempts                                  int32
	BuildSeconds                              int32
	HasLog                                    bool
	RequestedAt, AvailableAt                  time.Time
	StartedAt, FinishedAt                     time.Time
}

func refreshOf(row dbgen.BaselineRefresh) Refresh {
	return Refresh{
		Sequence: row.Sequence, Trigger: row.Trigger, State: row.State, Branch: row.Branch.String, CommitSHA: row.CommitSha.String,
		FailureCode: row.FailureCode.String, FailureDetail: row.FailureDetail.String, WaitingReason: row.WaitingReason.String,
		Attempts: row.Attempts, BuildSeconds: row.BuildSeconds.Int32, HasLog: row.LogLines > 0,
		RequestedAt: row.RequestedAt.Time, AvailableAt: row.AvailableAt.Time, StartedAt: row.StartedAt.Time, FinishedAt: row.FinishedAt.Time,
	}
}

// Overview is everything the Test data tab shows.
type Overview struct {
	Scope     Scope
	Settings  Settings
	Baselines []Baseline // current, one per SQL service
	Refreshes []Refresh  // newest first
	// Offer: what the workers can do for test data now.
	Offer Offer
}

// recentRefreshes is how many refreshes the tab lists.
const recentRefreshes = 5

// Service is the server side of the Test data tab.
type Service struct {
	store    *Store
	activity Activity
	catalog  Catalog
}

// NewService serves the Test data tab.
func NewService(store *Store, activity Activity, catalog Catalog) (*Service, error) {
	if store == nil || activity == nil || catalog == nil {
		return nil, errors.New("the test data tab requires a store, activity and the worker catalog")
	}
	return &Service{store: store, activity: activity, catalog: catalog}, nil
}

func (s *Service) scope(ctx context.Context, q *dbgen.Queries, user, repoID int64) (Scope, error) {
	if user <= 0 || repoID <= 0 {
		return Scope{}, ErrUnavailable
	}
	row, err := q.RepositoryScope(ctx, dbgen.RepositoryScopeParams{RepoID: repoID, UserID: user})
	if errors.Is(err, pgx.ErrNoRows) {
		return Scope{}, ErrUnavailable
	}
	if err != nil {
		return Scope{}, fmt.Errorf("read the repository: %w", err)
	}
	return Scope{RepoID: row.ID, FullName: row.FullName, Available: row.Available, CanManage: row.CanManage}, nil
}

// Overview reads the tab for someone who may build in the repository.
func (s *Service) Overview(ctx context.Context, user, repoID int64) (Overview, error) {
	q := dbgen.New(s.store.pool)
	scope, err := s.scope(ctx, q, user, repoID)
	if err != nil {
		return Overview{}, err
	}
	settings, err := s.store.Settings(ctx, repoID)
	if err != nil {
		return Overview{}, err
	}
	baselines, err := s.store.Current(ctx, repoID)
	if err != nil {
		return Overview{}, err
	}
	rows, err := q.LatestRefreshes(ctx, dbgen.LatestRefreshesParams{RepoID: repoID, Limit: recentRefreshes})
	if err != nil {
		return Overview{}, fmt.Errorf("read baseline refreshes: %w", err)
	}
	refreshes := make([]Refresh, len(rows))
	for i, row := range rows {
		refreshes[i] = refreshOf(row)
	}
	offer, err := s.catalog.TestData(ctx)
	if err != nil {
		return Overview{}, err
	}
	return Overview{Scope: scope, Settings: settings, Baselines: baselines, Refreshes: refreshes, Offer: offer}, nil
}

// Form is a submitted settings form. Revision is the settings revision the
// form showed (0 before the first save).
type Form struct {
	Revision                                    int64
	Enabled, KeepData, UseBaseline, AutoRefresh bool
	Branch                                      string
}

// Save stores an Owner's or Admin's settings; they can be prepared while
// Test data is off. Turning Test data on, turning the baseline on, or
// choosing another branch with automatic refresh, asks for a refresh;
// turning the baseline off cancels a waiting one. Turning Test data off is
// TurnOff, which deletes baselines.
func (s *Service) Save(ctx context.Context, user, repoID int64, form Form) (Settings, error) {
	branch, err := ValidBranch(form.Branch)
	if err != nil {
		return Settings{}, err
	}
	var saved Settings
	err = database.InTransaction(ctx, s.store.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		scope, err := s.scope(ctx, q, user, repoID)
		if err != nil {
			return err
		}
		if !scope.CanManage {
			return ErrForbidden
		}
		if form.Enabled && !scope.Available {
			return ErrAccessRemoved
		}
		before, err := lockSettings(ctx, q, repoID)
		if err != nil {
			return err
		}
		if before.Revision != form.Revision || before.Enabled && !form.Enabled {
			return ErrConflict
		}
		row, err := q.SaveSettings(ctx, dbgen.SaveSettingsParams{
			RepoID: repoID, Enabled: form.Enabled, KeepData: form.KeepData, UseBaseline: form.UseBaseline,
			BaselineBranch: text(branch), AutoRefresh: form.AutoRefresh, UpdatedBy: user,
		})
		if err != nil {
			return fmt.Errorf("save test data settings: %w", err)
		}
		saved = settingsOf(row)
		wasUsing, using := before.Enabled && before.UseBaseline, saved.Enabled && saved.UseBaseline
		switch {
		case using && !wasUsing:
			err = s.store.requestTx(ctx, tx, repoID, TriggerEnabled, user)
		case using && saved.AutoRefresh && before.Branch != saved.Branch:
			err = s.store.requestTx(ctx, tx, repoID, TriggerBranch, user)
		case !using && wasUsing:
			err = q.CancelWaitingRefreshes(ctx, repoID)
		}
		if err != nil {
			return fmt.Errorf("follow the settings change: %w", err)
		}
		metadata := map[string]any{"enabled": saved.Enabled, "keep_data": saved.KeepData, "use_baseline": saved.UseBaseline, "auto_refresh": saved.AutoRefresh}
		if saved.Branch != "" {
			metadata["branch"] = saved.Branch
		}
		return s.activity.RecordTx(ctx, tx, Event{RepoID: repoID, ActorID: user, Kind: KindSettingsUpdated, Metadata: metadata,
			DedupKey: fmt.Sprintf("test-data:%d:settings:%d", repoID, saved.Revision)})
	})
	return saved, err
}

// TurnOff turns Test data off for an Owner or Admin: the repository's
// baselines are retired (their files are deleted shortly after) and waiting
// refreshes are cancelled. It returns how many baselines were retired.
func (s *Service) TurnOff(ctx context.Context, user, repoID, revision int64) (int64, error) {
	var retired int64
	err := database.InTransaction(ctx, s.store.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		scope, err := s.scope(ctx, q, user, repoID)
		if err != nil {
			return err
		}
		if !scope.CanManage {
			return ErrForbidden
		}
		before, err := lockSettings(ctx, q, repoID)
		if err != nil {
			return err
		}
		if before.Revision != revision {
			return ErrConflict
		}
		row, err := q.SaveSettings(ctx, dbgen.SaveSettingsParams{
			RepoID: repoID, Enabled: false, KeepData: before.KeepData, UseBaseline: before.UseBaseline,
			BaselineBranch: text(before.Branch), AutoRefresh: before.AutoRefresh, UpdatedBy: user,
		})
		if err != nil {
			return fmt.Errorf("turn test data off: %w", err)
		}
		if retired, err = q.RetireRepositoryBaselines(ctx, repoID); err != nil {
			return fmt.Errorf("retire baselines: %w", err)
		}
		if err := q.CancelWaitingRefreshes(ctx, repoID); err != nil {
			return fmt.Errorf("cancel waiting refreshes: %w", err)
		}
		return s.activity.RecordTx(ctx, tx, Event{RepoID: repoID, ActorID: user, Kind: KindSettingsUpdated,
			Metadata: map[string]any{"enabled": false, "baselines_deleted": retired}, DedupKey: fmt.Sprintf("test-data:%d:settings:%d", repoID, row.Revision)})
	})
	return retired, err
}

// Refresh asks for a baseline refresh (Refresh baseline): Owners, Admins and
// Developers with a repository grant, while the repository uses a baseline.
func (s *Service) Refresh(ctx context.Context, user, repoID int64) error {
	return database.InTransaction(ctx, s.store.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		scope, err := s.scope(ctx, q, user, repoID)
		if err != nil {
			return err
		}
		if !scope.Available {
			return ErrAccessRemoved
		}
		settings, err := lockSettings(ctx, q, repoID)
		if err != nil {
			return err
		}
		if !settings.Enabled || !settings.UseBaseline {
			return ErrNoBaseline
		}
		return s.store.requestTx(ctx, tx, repoID, TriggerButton, user)
	})
}

// Delete retires an Owner's or Admin's current baselines (Delete baseline).
// Candidates fall back to empty databases until the next refresh. It returns
// how many were retired.
func (s *Service) Delete(ctx context.Context, user, repoID int64) (int64, error) {
	var retired int64
	err := database.InTransaction(ctx, s.store.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		scope, err := s.scope(ctx, q, user, repoID)
		if err != nil {
			return err
		}
		if !scope.CanManage {
			return ErrForbidden
		}
		if _, err := q.LockRepository(ctx, repoID); err != nil {
			return fmt.Errorf("lock the repository: %w", err)
		}
		if retired, err = q.RetireRepositoryBaselines(ctx, repoID); err != nil || retired == 0 {
			return err
		}
		return s.activity.RecordTx(ctx, tx, Event{RepoID: repoID, ActorID: user, Kind: KindBaselineDeleted, Metadata: map[string]any{"baselines": retired}})
	})
	return retired, err
}

// lockSettings serializes changes: the repository row, then its settings
// (the defaults before the first save). A missing repository is
// pgx.ErrNoRows.
func lockSettings(ctx context.Context, q *dbgen.Queries, repoID int64) (Settings, error) {
	if _, err := q.LockRepository(ctx, repoID); err != nil {
		return Settings{}, fmt.Errorf("lock the repository: %w", err)
	}
	row, err := q.LockSettings(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read test data settings: %w", err)
	}
	return settingsOf(row), nil
}

// ValidBranch checks a baseline branch name as Git would accept it ("" is
// the GitHub default branch) and returns it trimmed.
func ValidBranch(name string) (string, error) {
	name = strings.TrimSpace(name)
	invalid := func(message string) (string, error) {
		return "", &InvalidError{Field: "baseline_branch", Message: message}
	}
	switch {
	case name == "":
		return "", nil
	case len(name) > 255:
		return invalid("Use at most 255 characters.")
	case strings.HasPrefix(name, "refs/"):
		return invalid("Enter the branch name without refs/heads/, for example develop.")
	case strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") || name == "@":
		return invalid("This is not a valid Git branch name.")
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune(`~^:?*[\`, r) {
			return invalid("Branch names cannot contain spaces or any of ~ ^ : ? * [ \\.")
		}
	}
	return name, nil
}
