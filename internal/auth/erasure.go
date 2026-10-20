package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EraseAccountTx retains only the pseudonymous row required by shared authorship
// and the immutable provider ID that prevents deleted identities reactivating.
func (r *Repository) EraseAccountTx(ctx context.Context, tx pgx.Tx, user int64) error {
	for _, query := range []string{
		`DELETE FROM user_avatars WHERE user_id=$1`,
		`UPDATE users SET username='Deleted member',avatar_url='',first_name='',last_name='',github_name='',email='',email_sync_error=false,
		google_email='',github_profile_url='',github_bio='',github_company='',github_location='',github_created_at=NULL,profile_synced_at=NULL,custom_avatar=false,
		profile_revision=profile_revision+1,avatar_version=avatar_version+1 WHERE id=$1 AND deleted_at IS NOT NULL`,
	} {
		if _, err := tx.Exec(ctx, query, user); err != nil {
			return fmt.Errorf("erase personal profile: %w", err)
		}
	}
	return nil
}
