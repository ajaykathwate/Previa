package workspaces

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // Accept JPEG uploads; stored logos are re-encoded PNGs.
	"image/png"
	"strings"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/workspaces/dbgen"
	"github.com/jackc/pgx/v5"
)

const MaxLogoBytes = 192 << 10

var ErrLogo = errors.New("choose a PNG or JPEG logo up to 192 KiB and 512 by 512 pixels")

func (s *Service) UpdateProfile(ctx context.Context, user, id, revision int64, name, description string) error {
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if !validName(name) || utf8.RuneCountInString(description) > 240 || strings.ContainsRune(description, '\x00') {
		return ErrInput
	}
	return s.profileMutation(ctx, user, id, revision, func(q *dbgen.Queries) error {
		if err := q.UpdateWorkspaceProfile(ctx, dbgen.UpdateWorkspaceProfileParams{ID: id, Name: name, Description: description}); err != nil {
			return fmt.Errorf("save workspace details: %w", err)
		}
		return nil
	})
}

func (s *Service) profileMutation(ctx context.Context, user, id, revision int64, fn func(*dbgen.Queries) error) error {
	if revision <= 0 {
		return ErrInput
	}
	return database.InTransaction(ctx, s.repo.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockWorkspace(ctx, id); err != nil {
			return accessError(err)
		}
		w, err := q.ReadWorkspace(ctx, dbgen.ReadWorkspaceParams{UserID: user, WorkspaceID: id})
		if err != nil {
			return accessError(err)
		}
		if w.Role != "owner" && w.Role != "admin" {
			return ErrUnavailable
		}
		if w.ProfileRevision != revision {
			return ErrConflict
		}
		return fn(q)
	})
}

func (s *Service) SetLogo(ctx context.Context, user, id, revision int64, upload []byte) error {
	// Check local authority before spending CPU decoding an untrusted image.
	w, err := s.Read(ctx, user, id)
	if err != nil {
		return err
	}
	if !w.Manage() {
		return ErrUnavailable
	}
	data, err := normalizeLogo(upload)
	if err != nil {
		return err
	}
	return s.profileMutation(ctx, user, id, revision, func(q *dbgen.Queries) error {
		if err := q.SaveWorkspaceLogo(ctx, dbgen.SaveWorkspaceLogoParams{WorkspaceID: id, Version: revision + 1, Data: data}); err != nil {
			return fmt.Errorf("save workspace logo: %w", err)
		}
		if err := q.AdvanceWorkspaceProfile(ctx, id); err != nil {
			return fmt.Errorf("advance workspace profile: %w", err)
		}
		return nil
	})
}

func (s *Service) RemoveLogo(ctx context.Context, user, id, revision int64) error {
	return s.profileMutation(ctx, user, id, revision, func(q *dbgen.Queries) error {
		if err := q.DeleteWorkspaceLogo(ctx, id); err != nil {
			return fmt.Errorf("remove workspace logo: %w", err)
		}
		if err := q.AdvanceWorkspaceProfile(ctx, id); err != nil {
			return fmt.Errorf("advance workspace profile: %w", err)
		}
		return nil
	})
}

func (s *Service) Logo(ctx context.Context, user, id int64) ([]byte, int64, error) {
	v, err := dbgen.New(s.repo.pool).ReadWorkspaceLogo(ctx, dbgen.ReadWorkspaceLogoParams{ID: id, UserID: user})
	if err != nil {
		return nil, 0, accessError(err)
	}
	return v.Data, v.Version, nil
}

func normalizeLogo(upload []byte) ([]byte, error) {
	if len(upload) == 0 || len(upload) > MaxLogoBytes {
		return nil, ErrLogo
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(upload))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 512 || cfg.Height > 512 {
		return nil, ErrLogo
	}
	decoded, _, err := image.Decode(bytes.NewReader(upload))
	if err != nil {
		return nil, ErrLogo
	}
	var out bytes.Buffer
	if err := png.Encode(&out, decoded); err != nil {
		return nil, fmt.Errorf("normalize workspace logo: %w", err)
	}
	return out.Bytes(), nil
}
