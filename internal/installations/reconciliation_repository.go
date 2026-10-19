package installations

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/jackc/pgx/v5"
)

// InstallationSnapshot is captured before external reads. Revision is the
// compare-and-swap fence shared with signed deliveries and verified linking.
type InstallationSnapshot struct {
	ID, Revision int64
	Status       string
	Repositories []RepositoryInfo
	Observed     time.Time
}

type ReconciliationChange struct {
	InstallationSnapshot
	Status  string
	Removed []RepositoryInfo
}

func (r *Repository) ReconciliationSnapshot(ctx context.Context, target int64) ([]InstallationSnapshot, int, error) {
	var total int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM installations WHERE status IN ('active','suspended') AND disconnected_at IS NULL`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reconciliation installations: %w", err)
	}
	rows, err := r.db.Query(ctx, `SELECT i.github_installation_id,i.revision,i.status,clock_timestamp(),
	 coalesce((SELECT jsonb_agg(jsonb_build_object('ID',r.github_repo_id,'FullName',r.full_name,'Accessible',true)) FROM repos r WHERE r.installation_id=i.id AND r.accessible AND r.github_repo_id IS NOT NULL),'[]')
	 FROM installations i WHERE i.status IN ('active','suspended') AND i.disconnected_at IS NULL AND ($1::bigint=0 OR i.github_installation_id=$1)
	 ORDER BY (i.reconcile_requested_at>i.reconciled_at) DESC,i.reconciled_at,i.id LIMIT 25`, target)
	if err != nil {
		return nil, 0, fmt.Errorf("read reconciliation snapshots: %w", err)
	}
	defer rows.Close()
	var snapshots []InstallationSnapshot
	for rows.Next() {
		var s InstallationSnapshot
		var repos []byte
		if err = rows.Scan(&s.ID, &s.Revision, &s.Status, &s.Observed, &repos); err != nil {
			return nil, 0, fmt.Errorf("scan reconciliation snapshot: %w", err)
		}
		if err = json.Unmarshal(repos, &s.Repositories); err != nil {
			return nil, 0, fmt.Errorf("decode reconciliation repositories: %w", err)
		}
		snapshots = append(snapshots, s)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read reconciliation rows: %w", err)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	return snapshots, total, nil
}

// LinkConflicts returns only history in the verified caller's current workspace.
// It never grants authority based on a repository name or GitHub response.
func (r *Repository) LinkConflicts(ctx context.Context, user, newInstallation int64, repos []RepositoryInfo) ([]int64, error) {
	ids := make([]int64, 0, len(repos))
	for _, v := range repos {
		ids = append(ids, v.ID)
	}
	rows, err := r.db.Query(ctx, `SELECT DISTINCT i.github_installation_id FROM repos r JOIN installations i ON i.id=r.installation_id
	 WHERE r.github_repo_id=ANY($1::bigint[]) AND i.github_installation_id<>$2 AND i.status='active'
	 AND i.disconnected_at IS NULL AND i.workspace_id=CASE WHEN $3::bigint>0 THEN $3 ELSE workspace_default($4) END
	 AND workspace_repository_access(r.id,$4,'manage') ORDER BY i.github_installation_id`, ids, newInstallation, scope.WorkspaceID(ctx), user)
	if err != nil {
		return nil, fmt.Errorf("read same-workspace installation conflicts: %w", err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("scan installation conflicts: %w", err)
	}
	return values, nil
}
