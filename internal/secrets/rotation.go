package secrets

import (
	"context"
	"errors"
	"fmt"
)

// StoredSecret is one ciphertext row considered for re-encryption.
type StoredSecret struct {
	ID     int64
	RepoID int64
	SealedSecret
}

// RotationStore walks and rewrites ciphertext during key rotation.
type RotationStore interface {
	NeedingRotation(ctx context.Context, activeVersion int32, afterID int64, limit int32) ([]StoredSecret, error)
	// Rotate replaces a value only if it is unchanged since it was read, so a
	// concurrent owner edit (already on the active key) always wins.
	Rotate(ctx context.Context, s StoredSecret, sealed []byte, version int32) (bool, error)
	CountByVersion(ctx context.Context) (map[int32]int64, error)
}

// RotationReport summarizes one re-encryption run. Remaining counts values still
// not on the active key; remove an old key only when its count is zero.
type RotationReport struct {
	Rotated   int
	Skipped   int // changed concurrently; a later run picks them up if needed
	Remaining map[int32]int64
}

// Reencrypt moves every value onto the active key, in batches. It is safe to run
// while the server accepts edits and safe to repeat. It stops at the first value
// it cannot open, so a missing old key is reported instead of losing data.
func Reencrypt(ctx context.Context, store RotationStore, keys *Keyring, batch int32) (RotationReport, error) {
	if store == nil || keys == nil || batch <= 0 {
		return RotationReport{}, errors.New("re-encryption needs a store, a keyring and a positive batch size")
	}
	report := RotationReport{}
	active := keys.ActiveVersion()
	var after int64
	for {
		rows, err := store.NeedingRotation(ctx, active, after, batch)
		if err != nil {
			return report, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			plain, err := keys.Open(row.RepoID, row.Name, row.Version, row.Value)
			if err != nil {
				return report, fmt.Errorf("secret row %d (key version %d): %w", row.ID, row.Version, err)
			}
			sealed, version, err := keys.Seal(row.RepoID, row.Name, plain)
			clear(plain)
			if err != nil {
				return report, err
			}
			ok, err := store.Rotate(ctx, row, sealed, version)
			if err != nil {
				return report, err
			}
			if ok {
				report.Rotated++
			} else {
				report.Skipped++
			}
			after = row.ID
		}
	}
	counts, err := store.CountByVersion(ctx)
	if err != nil {
		return report, err
	}
	report.Remaining = map[int32]int64{}
	for version, n := range counts {
		if version != active && n > 0 {
			report.Remaining[version] = n
		}
	}
	return report, nil
}
