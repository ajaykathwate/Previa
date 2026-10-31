package reviews

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/reviews/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAccessName binds a sealed test-access secret to its purpose, so it can
// never be opened as a repository secret (whose names are environment
// variable names) or the other way round.
const TestAccessName = "previa:test-access"

const (
	maxAccountBytes = 300
	maxSecretBytes  = 512
)

// ErrNoTestAccess: nothing is stored to reveal.
var ErrNoTestAccess = errors.New("no test access secret")

// TestAccessInput saves how reviewers sign in to a repository's previews.
// An empty Secret keeps the stored one; RemoveSecret clears it. With no
// account and no secret left, the test access is removed.
type TestAccessInput struct {
	Account      string
	Secret       string
	RemoveSecret bool
	Revision     int64 // the revision the form was opened at; 0 when none
}

// SaveTestAccess stores the repository's test access: Owners and Admins only.
func (s *Service) SaveTestAccess(ctx context.Context, a Actor, in TestAccessInput) error {
	account, problem := cleanText(in.Account, maxAccountBytes)
	switch {
	case problem != "":
		return &InputError{Fields: map[string]string{"account": problem}, Message: "Check the highlighted fields."}
	case strings.Contains(account, "\n"):
		return &InputError{Fields: map[string]string{"account": "Keep it to one line, such as an email address."}, Message: "Check the highlighted fields."}
	}
	if problem := secretProblem(in.Secret); problem != "" {
		return &InputError{Fields: map[string]string{"secret": problem}, Message: "Check the highlighted fields."}
	}
	if in.Secret != "" && s.opts.Sealer == nil {
		return &InputError{Fields: map[string]string{"secret": "This server cannot store passwords: secret encryption is not set up."}, Message: "Save the account without a password, or ask the operator to set up secret encryption."}
	}
	return s.write(ctx, a, func(sc *scope) error {
		if !sc.manage {
			return ErrUnavailable
		}
		current, err := sc.q.LockTestAccess(ctx, a.RepoID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock the test access: %w", err)
		}
		if current.Revision != in.Revision {
			return &InputError{Message: "Someone changed the test access meanwhile; this is its latest version. Enter your change again."}
		}
		sealed, version, change := current.SecretCiphertext, current.KeyVersion, "kept"
		switch {
		case in.Secret != "":
			value := []byte(in.Secret)
			box, v, err := s.opts.Sealer.Seal(a.RepoID, TestAccessName, value)
			clear(value)
			if err != nil {
				return fmt.Errorf("seal the test access: %w", err)
			}
			sealed, version, change = box, pgtype.Int4{Int32: v, Valid: true}, "changed"
		case in.RemoveSecret:
			sealed, version, change = nil, pgtype.Int4{}, "removed"
		case sealed == nil:
			change = "none"
		}
		if account == "" && sealed == nil {
			if current.Revision == 0 {
				return nil // nothing stored, nothing asked for
			}
			if err := sc.q.DeleteTestAccess(ctx, a.RepoID); err != nil {
				return fmt.Errorf("remove the test access: %w", err)
			}
			change = "removed"
		} else if account == current.Account && change == "kept" {
			return nil // a repeated save of what is already there
		} else if _, err := sc.q.SaveTestAccess(ctx, dbgen.SaveTestAccessParams{
			RepoID: a.RepoID, Account: account, SecretCiphertext: sealed, KeyVersion: version, UpdatedBy: a.UserID,
		}); err != nil {
			return fmt.Errorf("save the test access: %w", err)
		}
		sc.wrote = true
		return s.record(ctx, sc, Event{Kind: KindTestAccessUpdated, Metadata: map[string]any{"secret": change}})
	})
}

// RevealTestAccess opens the repository's test-access secret for a member
// with preview access, recording who revealed it and when. The value is for
// the caller's response only.
func (s *Service) RevealTestAccess(ctx context.Context, a Actor) (string, error) {
	if s.opts.Sealer == nil {
		return "", ErrNoTestAccess
	}
	var value string
	err := s.write(ctx, a, func(sc *scope) error {
		current, err := sc.q.LockTestAccess(ctx, a.RepoID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && current.SecretCiphertext == nil {
			return ErrNoTestAccess
		}
		if err != nil {
			return fmt.Errorf("read the test access: %w", err)
		}
		plain, err := s.opts.Sealer.Open(a.RepoID, TestAccessName, current.KeyVersion.Int32, current.SecretCiphertext)
		if err != nil {
			return fmt.Errorf("open the test access: %w", err)
		}
		value = string(plain)
		clear(plain)
		if err := sc.q.MarkTestAccessRevealed(ctx, dbgen.MarkTestAccessRevealedParams{UserID: a.UserID, RepoID: a.RepoID}); err != nil {
			return fmt.Errorf("record the reveal: %w", err)
		}
		sc.wrote = true
		return s.record(ctx, sc, Event{Kind: KindTestAccessRevealed})
	})
	if err != nil {
		return "", err
	}
	return value, nil
}

// secretProblem checks a test password or token: printable, one line,
// bounded. Surrounding spaces are kept: they may be part of it.
func secretProblem(secret string) string {
	if secret == "" {
		return ""
	}
	if !utf8.ValidString(secret) || len(secret) > maxSecretBytes {
		return "Use up to 512 bytes of plain text."
	}
	for _, r := range secret {
		if unicode.IsControl(r) {
			return "Remove line breaks and control characters."
		}
	}
	return ""
}

// SealedAccess is one stored test-access secret, for key rotation.
type SealedAccess struct {
	ID, RepoID int64
	Version    int32
	Value      []byte
}

// Rotation walks and rewrites sealed test access during key rotation, beside
// repository secrets; internal/app adapts it to the secrets feature's
// rotation store.
type Rotation struct{ pool *pgxpool.Pool }

// NewRotation returns the rotation store.
func NewRotation(pool *pgxpool.Pool) *Rotation { return &Rotation{pool: pool} }

// NeedingRotation lists sealed values not on the active key, after an ID.
func (r *Rotation) NeedingRotation(ctx context.Context, active int32, after int64, limit int32) ([]SealedAccess, error) {
	rows, err := dbgen.New(r.pool).TestAccessNeedingRotation(ctx, dbgen.TestAccessNeedingRotationParams{ActiveVersion: active, AfterID: after, BatchSize: limit})
	if err != nil {
		return nil, fmt.Errorf("list test access to rotate: %w", err)
	}
	out := make([]SealedAccess, len(rows))
	for i, row := range rows {
		out[i] = SealedAccess{ID: row.ID, RepoID: row.RepoID, Version: row.KeyVersion.Int32, Value: row.SecretCiphertext}
	}
	return out, nil
}

// Rotate replaces a sealed value only if it is unchanged since it was read.
func (r *Rotation) Rotate(ctx context.Context, previous SealedAccess, sealed []byte, version int32) (bool, error) {
	n, err := dbgen.New(r.pool).RotateTestAccess(ctx, dbgen.RotateTestAccessParams{ID: previous.ID, Sealed: sealed, KeyVersion: version, Previous: previous.Value})
	if err != nil {
		return false, fmt.Errorf("rotate test access: %w", err)
	}
	return n == 1, nil
}

// CountByVersion counts sealed values per key version.
func (r *Rotation) CountByVersion(ctx context.Context) (map[int32]int64, error) {
	rows, err := dbgen.New(r.pool).CountTestAccessByVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("count test access by key: %w", err)
	}
	counts := map[int32]int64{}
	for _, row := range rows {
		counts[row.KeyVersion] = row.Values
	}
	return counts, nil
}
