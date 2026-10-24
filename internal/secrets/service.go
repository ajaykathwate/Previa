package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// MaxValueBytes bounds one secret value.
	MaxValueBytes = 32 * 1024
	// MaxFileBytes bounds a repository's .env file.
	MaxFileBytes = 64 * 1024
	// MaxSecretsPerRepository bounds the names with values a file may set.
	MaxSecretsPerRepository = 500
	// ReservedPrefix is kept for variables the platform will inject itself.
	ReservedPrefix = "PREVIA_"
	// envFileName authenticates the sealed file. It can never be a secret's
	// name, so a value cannot be passed off as the file or the other way.
	envFileName = ".env"
)

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

var (
	// ErrNotConfigured: the server has no keyring, so values cannot be encrypted.
	ErrNotConfigured = errors.New("repository secrets are not configured on this server")
	// ErrUnavailable: the repository does not exist, is not owned by this user or,
	// for changes other than deletion, has lost its GitHub access.
	ErrUnavailable = errors.New("repository is unavailable or not owned by this user")
	// ErrLimit: the file sets more names than a repository may have.
	ErrLimit = errors.New("this repository already has the maximum number of secrets")
	// ErrChanged: someone saved the file after this editor opened it.
	ErrChanged = errors.New("the .env file changed since it was opened")
)

// InvalidError explains why a submitted name or value was rejected. It never
// contains the value. Entry is the 1-based line of the rejected entry in a
// .env file, or 0.
type InvalidError struct {
	Field   string
	Message string
	Entry   int
}

func (e *InvalidError) Error() string { return e.Message }

// RemovalsError means saving the file would remove these names; nothing
// changed. Saving again with the removal confirmed removes them.
type RemovalsError struct{ Names []string }

func (e *RemovalsError) Error() string {
	return "saving removes " + strings.Join(e.Names, ", ")
}

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

// SealedFile is a repository's .env file as stored. Revision 0 means no file
// was saved yet.
type SealedFile struct {
	Value     []byte
	Version   int32
	Revision  int64
	UpdatedAt time.Time
	UpdatedBy string
}

// FileSave replaces a repository's secrets and its file together: Keep is
// every name the file sets, Upsert the new and changed values among them.
type FileSave struct {
	File     SealedFile
	Expected int64 // the revision the editor opened
	Upsert   []SealedSecret
	Keep     []string
}

// Store persists ciphertext under owner-scoped queries.
type Store interface {
	Repository(ctx context.Context, userID, repoID int64) (RepositoryView, error)
	List(ctx context.Context, userID, repoID int64) ([]Summary, error)
	// EnvFile reads the repository's sealed file and secrets for an owner.
	EnvFile(ctx context.Context, userID, repoID int64) (SealedFile, []SealedSecret, error)
	// SaveEnvFile applies a FileSave all or nothing, only while the file is
	// still at the expected revision (ErrChanged otherwise), and returns the
	// new revision. New or changed values need current repository access.
	SaveEnvFile(ctx context.Context, userID, repoID int64, save FileSave) (int64, error)
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

// File is a repository's .env file as its Owners and Admins edit it.
type File struct {
	Text      []byte // plaintext; the caller clears it after use
	Revision  int64  // 0: no file was saved yet; Text lists the saved secrets
	UpdatedAt time.Time
	UpdatedBy string
}

// File opens the repository's .env file. Before the first save it is made from
// the repository's secrets, one NAME=value line each.
func (s *Service) File(ctx context.Context, userID, repoID int64) (File, error) {
	if s.keys == nil {
		return File{}, ErrNotConfigured
	}
	stored, sealed, err := s.store.EnvFile(ctx, userID, repoID)
	if err != nil {
		return File{}, err
	}
	if stored.Revision > 0 {
		text, err := s.keys.Open(repoID, envFileName, stored.Version, stored.Value)
		if err != nil {
			return File{}, fmt.Errorf("open .env file: %w", err)
		}
		return File{Text: text, Revision: stored.Revision, UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy}, nil
	}
	var text bytes.Buffer
	for _, secret := range sealed {
		value, err := s.keys.Open(repoID, secret.Name, secret.Version, secret.Value)
		if err != nil {
			return File{}, fmt.Errorf("open secret %s: %w", secret.Name, err)
		}
		text.WriteString(FormatDotenvLine(secret.Name, value) + "\n")
		clear(value)
	}
	return File{Text: text.Bytes()}, nil
}

// Saved reports, by name, what saving a file changed.
type Saved struct {
	Added, Changed, Removed []string
	Revision                int64
}

// SaveFile makes text the repository's .env file and its secrets exactly the
// names it sets: a name without a value (NAME=) stays in the file but is not
// set. revision is the one the editor opened; a save that would remove names
// needs confirmRemovals (RemovalsError otherwise). The caller clears text.
func (s *Service) SaveFile(ctx context.Context, userID, repoID int64, text []byte, revision int64, confirmRemovals bool) (Saved, error) {
	values, err := fileValues(text)
	defer func() {
		for _, value := range values {
			clear(value)
		}
	}()
	if err != nil {
		return Saved{}, err
	}
	if s.keys == nil {
		return Saved{}, ErrNotConfigured
	}
	stored, sealed, err := s.store.EnvFile(ctx, userID, repoID)
	if err != nil {
		return Saved{}, err
	}
	if stored.Revision != revision {
		return Saved{}, ErrChanged
	}
	var saved Saved
	var upsert []SealedSecret
	for _, secret := range sealed {
		current, err := s.keys.Open(repoID, secret.Name, secret.Version, secret.Value)
		if err != nil {
			return Saved{}, fmt.Errorf("open secret %s: %w", secret.Name, err)
		}
		value, kept := values[secret.Name]
		switch {
		case !kept:
			saved.Removed = append(saved.Removed, secret.Name)
		case !bytes.Equal(current, value):
			saved.Changed = append(saved.Changed, secret.Name)
		}
		clear(current)
	}
	if len(saved.Removed) > 0 && !confirmRemovals {
		return Saved{}, &RemovalsError{Names: saved.Removed}
	}
	keep := make([]string, 0, len(values))
	for name := range values {
		keep = append(keep, name)
	}
	slices.Sort(keep)
	for _, name := range keep {
		if !slices.ContainsFunc(sealed, func(secret SealedSecret) bool { return secret.Name == name }) {
			saved.Added = append(saved.Added, name)
		} else if !slices.Contains(saved.Changed, name) {
			continue
		}
		value, version, err := s.keys.Seal(repoID, name, values[name])
		if err != nil {
			return Saved{}, err
		}
		upsert = append(upsert, SealedSecret{Name: name, Value: value, Version: version})
	}
	file, version, err := s.keys.Seal(repoID, envFileName, text)
	if err != nil {
		return Saved{}, err
	}
	saved.Revision, err = s.store.SaveEnvFile(ctx, userID, repoID, FileSave{
		File: SealedFile{Value: file, Version: version}, Expected: revision, Upsert: upsert, Keep: keep,
	})
	if err != nil {
		return Saved{}, err
	}
	return saved, nil
}

// fileValues checks a .env file and returns the value of every name it sets.
// Errors name the line, never a value.
func fileValues(text []byte) (map[string][]byte, error) {
	if len(text) > MaxFileBytes {
		return nil, &InvalidError{Field: "dotenv", Message: "The .env file must not exceed 64 KiB."}
	}
	if bytes.IndexByte(text, 0) >= 0 {
		return nil, &InvalidError{Field: "dotenv", Message: "The .env file cannot contain NUL characters."}
	}
	entries, err := ParseDotenv(string(text))
	if err != nil {
		return nil, err
	}
	values := make(map[string][]byte, len(entries))
	lines := make(map[string]int, len(entries))
	for _, e := range entries {
		if err := ValidateName(e.Name); err != nil {
			return values, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: %s", e.Line, err.(*InvalidError).Message), Entry: e.Line}
		}
		if first, seen := lines[e.Name]; seen {
			return values, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: %s is already set on line %d. Keep one.", e.Line, e.Name, first), Entry: e.Line}
		}
		lines[e.Name] = e.Line
		if len(e.Value) == 0 {
			continue // NAME= leaves the name unset, as in any .env file
		}
		if err := validateValue(e.Value); err != nil {
			return values, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: %s: %s", e.Line, e.Name, err.(*InvalidError).Message), Entry: e.Line}
		}
		if e.Name == RegistryAuthName {
			if err := validateRegistryAuth(e.Value); err != nil {
				return values, &InvalidError{Field: "dotenv", Message: fmt.Sprintf("Line %d: %s", e.Line, err.Error()), Entry: e.Line}
			}
		}
		values[e.Name] = e.Value
	}
	if len(values) > MaxSecretsPerRepository {
		return values, ErrLimit
	}
	return values, nil
}

// Put sets one name in the repository's .env file, rewriting its line or
// adding one, and saves the file as the editor would. It reports whether the
// name is new. The caller should clear value afterwards.
func (s *Service) Put(ctx context.Context, userID, repoID int64, name string, value []byte) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	if err := validateValue(value); err != nil {
		return false, err
	}
	file, err := s.File(ctx, userID, repoID)
	if err != nil {
		return false, err
	}
	text, err := setDotenv(string(file.Text), name, value)
	clear(file.Text)
	if err != nil {
		return false, err
	}
	saved, err := s.SaveFile(ctx, userID, repoID, []byte(text), file.Revision, false)
	return len(saved.Added) == 1, err
}
