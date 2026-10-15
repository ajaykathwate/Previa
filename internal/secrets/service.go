package secrets

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxValueBytes bounds one secret value.
	MaxValueBytes = 32 * 1024
	// MaxSecretsPerRepository bounds a repository's secrets.
	MaxSecretsPerRepository = 100
	// ReservedPrefix is kept for variables the platform will inject itself.
	ReservedPrefix = "PREVIA_"
)

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

var (
	// ErrNotConfigured: the server has no keyring, so values cannot be encrypted.
	ErrNotConfigured = errors.New("repository secrets are not configured on this server")
	// ErrUnavailable: the repository does not exist, is not owned by this user or,
	// for changes other than deletion, has lost its GitHub access.
	ErrUnavailable = errors.New("repository is unavailable or not owned by this user")
	// ErrLimit: the repository already has the maximum number of secrets.
	ErrLimit = errors.New("this repository already has the maximum number of secrets")
)

// InvalidError explains why a submitted name or value was rejected. It never
// contains the value. Entry is the 1-based position of the rejected entry in a
// batch, or 0.
type InvalidError struct {
	Field   string
	Message string
	Entry   int
}

func (e *InvalidError) Error() string { return e.Message }

// Summary is what owners may see after saving: the name, never the value.
type Summary struct {
	Name      string
	UpdatedAt time.Time
	Build     bool // Dockerfile steps of trusted builds may read it
}

// RepositoryView identifies an owned repository for the secrets page.
type RepositoryView struct {
	ID        int64
	FullName  string
	Available bool // GitHub access is current; required to add or replace values
}

// Store persists ciphertext under owner-scoped queries.
type Store interface {
	Repository(ctx context.Context, userID, repoID int64) (RepositoryView, error)
	List(ctx context.Context, userID, repoID int64) ([]Summary, error)
	// PutMany creates or replaces values, all or nothing. It enforces
	// ownership, current access and the per-repository limit atomically and
	// reports how many names it created.
	PutMany(ctx context.Context, userID, repoID int64, items []SealedSecret) (int, error)
	// DeleteMany removes names and reports how many existed.
	DeleteMany(ctx context.Context, userID, repoID int64, names []string) (int, error)
	// SetBuild lets builds read a secret or stops it.
	SetBuild(ctx context.Context, userID, repoID int64, name string, build bool) (bool, error)
}

// Service manages repository secrets for their owners.
type Service struct {
	store Store
	keys  *Keyring
}

// NewService accepts a nil keyring: secrets are then visibly not configured.
func NewService(store Store, keys *Keyring) (*Service, error) {
	if store == nil {
		return nil, errors.New("secrets service requires a store")
	}
	return &Service{store: store, keys: keys}, nil
}

func (s *Service) Configured() bool { return s.keys != nil }

// SetBuild lets Dockerfile steps of trusted builds read the named secret, or
// stops it. Allowing needs current repository access, like saving a value.
func (s *Service) SetBuild(ctx context.Context, userID, repoID int64, name string, build bool) (bool, error) {
	if !namePattern.MatchString(name) {
		return false, &InvalidError{Field: "name", Message: "Unknown secret."}
	}
	if build {
		view, err := s.store.Repository(ctx, userID, repoID)
		if err != nil {
			return false, err
		}
		if !view.Available {
			return false, ErrUnavailable
		}
	}
	return s.store.SetBuild(ctx, userID, repoID, name, build)
}

func (s *Service) Repository(ctx context.Context, userID, repoID int64) (RepositoryView, error) {
	return s.store.Repository(ctx, userID, repoID)
}

func (s *Service) List(ctx context.Context, userID, repoID int64) ([]Summary, error) {
	return s.store.List(ctx, userID, repoID)
}

// ValidateName checks a secret name without touching storage.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return &InvalidError{Field: "name", Message: "Use letters, digits and underscores, starting with a letter or underscore (up to 128 characters)."}
	}
	if strings.HasPrefix(strings.ToUpper(name), ReservedPrefix) {
		return &InvalidError{Field: "name", Message: "Names starting with PREVIA_ are reserved for the platform."}
	}
	return nil
}

func validateValue(value []byte) error {
	switch {
	case len(value) == 0:
		return &InvalidError{Field: "value", Message: "Enter a value."}
	case len(value) > MaxValueBytes:
		return &InvalidError{Field: "value", Message: "Values must not exceed 32 KiB."}
	case bytes.IndexByte(value, 0) >= 0:
		return &InvalidError{Field: "value", Message: "Values cannot contain NUL characters."}
	}
	return nil
}

// Put encrypts value and stores it under name, replacing any existing value.
// The caller should clear value afterwards.
func (s *Service) Put(ctx context.Context, userID, repoID int64, name string, value []byte) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	if err := validateValue(value); err != nil {
		return false, err
	}
	created, err := s.PutMany(ctx, userID, repoID, []Entry{{Name: name, Value: value}})
	return created == 1, err
}

// PutMany encrypts entries and stores them together: all are saved or none.
// Names must be unique; existing names are replaced. It reports how many names
// were new. The caller should clear the values afterwards.
func (s *Service) PutMany(ctx context.Context, userID, repoID int64, entries []Entry) (int, error) {
	if len(entries) == 0 {
		return 0, &InvalidError{Message: "Add at least one secret with a name and a value."}
	}
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if err := ValidateName(e.Name); err != nil {
			invalid := err.(*InvalidError)
			return 0, &InvalidError{Field: "name", Message: invalid.Message, Entry: i + 1}
		}
		if seen[e.Name] {
			return 0, &InvalidError{Field: "name", Message: e.Name + " appears more than once. Keep one value per name.", Entry: i + 1}
		}
		seen[e.Name] = true
		if err := validateValue(e.Value); err != nil {
			invalid := err.(*InvalidError)
			return 0, &InvalidError{Field: "value", Message: e.Name + ": " + invalid.Message, Entry: i + 1}
		}
		if e.Name == RegistryAuthName {
			if err := validateRegistryAuth(e.Value); err != nil {
				return 0, &InvalidError{Field: "value", Message: err.Error(), Entry: i + 1}
			}
		}
	}
	if s.keys == nil {
		return 0, ErrNotConfigured
	}
	items := make([]SealedSecret, len(entries))
	for i, e := range entries {
		sealed, version, err := s.keys.Seal(repoID, e.Name, e.Value)
		if err != nil {
			return 0, err
		}
		items[i] = SealedSecret{Name: e.Name, Value: sealed, Version: version}
	}
	return s.store.PutMany(ctx, userID, repoID, items)
}

// Delete removes a secret. Owners may delete even after GitHub access was lost.
func (s *Service) Delete(ctx context.Context, userID, repoID int64, name string) (bool, error) {
	n, err := s.DeleteMany(ctx, userID, repoID, []string{name})
	return n == 1, err
}

// DeleteMany removes several secrets at once and reports how many existed.
// Owners may delete even after GitHub access was lost.
func (s *Service) DeleteMany(ctx context.Context, userID, repoID int64, names []string) (int, error) {
	unique := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !namePattern.MatchString(name) {
			return 0, &InvalidError{Field: "name", Message: "Unknown secret."}
		}
		if !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	if len(unique) == 0 {
		return 0, &InvalidError{Field: "name", Message: "Select at least one secret."}
	}
	return s.store.DeleteMany(ctx, userID, repoID, unique)
}
