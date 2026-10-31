package app

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/reviews"
	"github.com/ajaykathwatee/previa/internal/secrets"
)

// RunSecretsReencrypt moves every repository secret onto the active key version
// and reports how many values still use each older version. It prints counts
// only, never names or values.
func RunSecretsReencrypt(ctx context.Context, out io.Writer) error {
	cfg, err := config.LoadSecretsMaintenance()
	if err != nil {
		return err
	}
	keys, err := secrets.ParseKeyring(cfg.SecretsEncryptionKeys, cfg.SecretsActiveKeyVersion)
	if err != nil {
		return err
	}
	pool, err := database.NewPool(ctx, cfg.SharedConfig)
	if err != nil {
		return fmt.Errorf("initialize secrets database: %w", err)
	}
	defer pool.Close()
	// Repository secrets, then reviews' test-access secrets, sealed with the
	// same keys (docs/31-test-data.md).
	for _, pass := range []struct {
		label string
		store secrets.RotationStore
	}{
		{"Secrets", secrets.NewRepository(pool)},
		{"Test access", testAccessRotation{store: reviews.NewRotation(pool)}},
	} {
		if err := reencryptPass(ctx, out, pass.label, pass.store, keys); err != nil {
			return err
		}
	}
	return nil
}

// reencryptPass rotates one store and reports what still uses older keys.
func reencryptPass(ctx context.Context, out io.Writer, label string, store secrets.RotationStore, keys *secrets.Keyring) error {
	report, err := secrets.Reencrypt(ctx, store, keys, 100)
	if _, writeErr := fmt.Fprintf(out, "%s: re-encrypted %d, skipped %d (changed during the run), active key version %d\n", label, report.Rotated, report.Skipped, keys.ActiveVersion()); writeErr != nil && err == nil {
		err = writeErr
	}
	if err != nil {
		return fmt.Errorf("re-encrypt %s: %w", strings.ToLower(label), err)
	}
	versions := make([]int, 0, len(report.Remaining))
	for version := range report.Remaining {
		versions = append(versions, int(version))
	}
	sort.Ints(versions)
	for _, version := range versions {
		if _, err := fmt.Fprintf(out, "%s: %d value(s) still use key version %d; run again before removing it\n", label, report.Remaining[int32(version)], version); err != nil {
			return err
		}
	}
	if len(versions) == 0 {
		if _, err := fmt.Fprintf(out, "%s: every value uses the active key; older key versions can be removed\n", label); err != nil {
			return err
		}
	}
	return nil
}
