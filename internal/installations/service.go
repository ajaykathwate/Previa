package installations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-github/v91/github"
)

var (
	ErrForbidden          = errors.New("installation linking is not authorized")
	ErrSuperseded         = errors.New("installation changed while linking; start again")
	ErrCleanupUnavailable = errors.New("installation environment cleanup adapter is not configured")
)

type RepositoryInfo struct {
	ID         int64
	FullName   string
	Accessible bool
}
type InstallationInfo struct {
	ID           int64
	Account      string
	Status       string
	Repositories []RepositoryInfo
}

type store interface {
	Prepare(context.Context, int64) (int64, error)
	Summary(context.Context, int64) (WorkspaceSummary, error)
	Page(context.Context, int64, RepositoryFilter) (RepositoryPage, error)
	Link(context.Context, int64, int64, int64, string, []RepositoryInfo) error
	List(context.Context, int64) ([]InstallationInfo, error)
	ApplyEvent(context.Context, int64, string, string, []RepositoryInfo, []RepositoryInfo, bool) error
}

type appClient interface {
	Installation(context.Context, int64) (*github.Installation, error)
	InstallationClient(int64) (*github.Client, error)
	Invalidate(int64)
}

type userAuthorization interface {
	AuthCodeURL(string) string
	Verify(context.Context, string, int64, int64) ([]RepositoryInfo, error)
}

// CleanupRequester marks the future teardown boundary. No runtime/job adapter is
// wired in this phase. Calls must be idempotent because events may be retried.
type CleanupRequester interface {
	RequestInstallationCleanup(context.Context, int64) error
}

type Service struct {
	store         store
	app           appClient
	authorization userAuthorization
	cleanup       CleanupRequester
	reconciler    interface {
		ResolveLinkConflicts(context.Context, int64, int64, []RepositoryInfo) error
	}
}

func NewService(store store, app appClient, authorization userAuthorization, cleanup CleanupRequester) *Service {
	return &Service{store: store, app: app, authorization: authorization, cleanup: cleanup}
}

func (s *Service) WithReconciler(r interface {
	ResolveLinkConflicts(context.Context, int64, int64, []RepositoryInfo) error
}) *Service { s.reconciler = r; return s }

func (s *Service) WithAdmission(g AdmissionGate) *Service {
	if r, ok := s.store.(*Repository); ok {
		r.admission = g
	}
	return s
}
func (s *Service) List(ctx context.Context, userID int64) ([]InstallationInfo, error) {
	return s.store.List(ctx, userID)
}

// Link performs external verification before a short atomic persistence step.
// A revision fence prevents lifecycle events from being overwritten by this API snapshot.
func (s *Service) Link(ctx context.Context, code string, userID, githubUserID, installationID int64) error {
	if s.app == nil || s.authorization == nil {
		return errors.New("GitHub App linking is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	revision, err := s.store.Prepare(ctx, installationID)
	if err != nil {
		return fmt.Errorf("prepare installation: %w", err)
	}
	repos, err := s.authorization.Verify(ctx, code, githubUserID, installationID)
	if err != nil {
		return fmt.Errorf("verify installation user access: %w", err)
	}
	installation, err := s.app.Installation(ctx, installationID)
	if err != nil {
		return fmt.Errorf("verify App installation: %w", err)
	}
	if installation.GetID() != installationID || installation.SuspendedAt != nil {
		return ErrForbidden
	}
	// Prove installation-token minting and repo metadata access without exposing
	// the token. Only repositories accessible to the verified user are recorded.
	client, err := s.app.InstallationClient(installationID)
	if err != nil {
		return fmt.Errorf("create installation client: %w", err)
	}
	if _, _, err := client.Apps.ListRepos(ctx, &github.ListOptions{PerPage: 1}); err != nil {
		return fmt.Errorf("verify installation token access: %w", err)
	}
	if len(repos) > 0 {
		repo, _, err := client.Repositories.GetByID(ctx, repos[0].ID)
		if err != nil {
			return fmt.Errorf("verify installation repository access: %w", err)
		}
		if repo.GetID() != repos[0].ID {
			return ErrForbidden
		}
	}
	if s.reconciler != nil {
		if err := s.reconciler.ResolveLinkConflicts(ctx, userID, installationID, repos); err != nil {
			return fmt.Errorf("verify previous installation before relinking: %w", err)
		}
	}
	if err := s.store.Link(ctx, installationID, userID, revision, installation.GetAccount().GetLogin(), repos); err != nil {
		return fmt.Errorf("link installation: %w", err)
	}
	return nil
}

// HandleEvent accepts already authenticated/parsed GitHub payloads; it is not an
// HTTP receiver. A future receiver must verify signature and delivery provenance.
func (s *Service) HandleEvent(ctx context.Context, event any) error {
	if s.app == nil {
		return errors.New("GitHub App lifecycle client is not configured")
	}
	change, err := ParseLifecycleEvent(event)
	if err != nil {
		return err
	}
	id, status, account := change.ID, change.Status, change.Account
	added, removed, replace, cleanup := change.Added, change.Removed, change.Replace, change.Cleanup
	if err := s.store.ApplyEvent(ctx, id, status, account, added, removed, replace); err != nil {
		return fmt.Errorf("apply installation lifecycle: %w", err)
	}
	s.app.Invalidate(id)
	if cleanup && s.cleanup == nil {
		return ErrCleanupUnavailable
	}
	if cleanup && s.cleanup != nil {
		if err := s.cleanup.RequestInstallationCleanup(ctx, id); err != nil {
			return fmt.Errorf("request installation cleanup: %w", err)
		}
	}
	return nil
}
func eventRepositories(repos []*github.Repository) []RepositoryInfo {
	result := make([]RepositoryInfo, 0, len(repos))
	for _, repo := range repos {
		result = append(result, RepositoryInfo{ID: repo.GetID(), FullName: repo.GetFullName(), Accessible: true})
	}
	return result
}

// LifecycleChange is the normalized, validated lifecycle mutation.
type LifecycleChange struct {
	ID               int64
	Status, Account  string
	Added, Removed   []RepositoryInfo
	Replace, Cleanup bool
}

func ParseLifecycleEvent(event any) (LifecycleChange, error) {
	var id int64
	var account, status string
	var added, removed []RepositoryInfo
	var replace, cleanup bool
	switch e := event.(type) {
	case *github.InstallationEvent:
		if e == nil || e.Installation == nil {
			return LifecycleChange{}, errors.New("installation event is missing installation")
		}
		id = e.Installation.GetID()
		account = e.Installation.GetAccount().GetLogin()
		switch e.GetAction() {
		case "created":
			status = "active"
			replace = true
			added = eventRepositories(e.Repositories)
		case "deleted":
			status = "inactive"
			cleanup = true
		case "suspend":
			status = "suspended"
			cleanup = true
		case "unsuspend":
			status = "active"
		case "new_permissions_accepted":
			status = ""
		default:
			return LifecycleChange{}, errors.New("unsupported installation action")
		}
	case *github.InstallationRepositoriesEvent:
		if e == nil || e.Installation == nil {
			return LifecycleChange{}, errors.New("repository event is missing installation")
		}
		if e.GetAction() != "added" && e.GetAction() != "removed" {
			return LifecycleChange{}, errors.New("unsupported installation repositories action")
		}
		id = e.Installation.GetID()
		account = e.Installation.GetAccount().GetLogin()
		added = eventRepositories(e.RepositoriesAdded)
		removed = eventRepositories(e.RepositoriesRemoved)
	default:
		return LifecycleChange{}, errors.New("unsupported installation event type")
	}
	if id <= 0 {
		return LifecycleChange{}, errors.New("installation ID must be positive")
	}
	for _, repo := range append(append([]RepositoryInfo{}, added...), removed...) {
		if repo.ID <= 0 || repo.FullName == "" {
			return LifecycleChange{}, errors.New("event repository is invalid")
		}
	}
	return LifecycleChange{id, status, account, added, removed, replace, cleanup}, nil
}
