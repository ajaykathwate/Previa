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
// contains the value.
type InvalidError struct {
	Field   string
	Message string
}

func (e *InvalidError) Error() string { return e.Message }

// Summary is what owners may see after saving: the name, never the value.
type Summary struct {
	Name      string
	UpdatedAt time.Time
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
	// Put creates or replaces a value. It enforces ownership, current access and
	// the per-repository limit atomically and reports whether it created a name.
	Put(ctx context.Context, userID, repoID int64, name string, sealed []byte, version int32) (bool, error)
	Delete(ctx context.Context, userID, repoID int64, name string) (bool, error)
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
	if s.keys == nil {
		return false, ErrNotConfigured
	}
	sealed, version, err := s.keys.Seal(repoID, name, value)
	if err != nil {
		return false, err
	}
	return s.store.Put(ctx, userID, repoID, name, sealed, version)
}

// Delete removes a secret. Owners may delete even after GitHub access was lost.
func (s *Service) Delete(ctx context.Context, userID, repoID int64, name string) (bool, error) {
	if !namePattern.MatchString(name) {
		return false, &InvalidError{Field: "name", Message: "Unknown secret."}
	}
	return s.store.Delete(ctx, userID, repoID, name)
}
