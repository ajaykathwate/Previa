package app

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/database"
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
	report, err := secrets.Reencrypt(ctx, secrets.NewRepository(pool), keys, 100)
	if _, writeErr := fmt.Fprintf(out, "Secrets: re-encrypted %d, skipped %d (changed during the run), active key version %d\n", report.Rotated, report.Skipped, keys.ActiveVersion()); writeErr != nil && err == nil {
		err = writeErr
	}
	if err != nil {
		return fmt.Errorf("re-encrypt secrets: %w", err)
	}
	versions := make([]int, 0, len(report.Remaining))
	for version := range report.Remaining {
		versions = append(versions, int(version))
	}
	sort.Ints(versions)
	for _, version := range versions {
		if _, err := fmt.Fprintf(out, "Secrets: %d value(s) still use key version %d; run again before removing it\n", report.Remaining[int32(version)], version); err != nil {
			return err
		}
	}
	if len(versions) == 0 {
		if _, err := fmt.Fprintln(out, "Secrets: every value uses the active key; older key versions can be removed"); err != nil {
			return err
		}
	}
	return nil
}
