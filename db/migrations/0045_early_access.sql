-- +goose Up
-- Existing workspaces remain approved; new workspaces await operator approval.
ALTER TABLE workspaces ADD COLUMN early_access_state text NOT NULL DEFAULT 'approved' CHECK(early_access_state IN ('pending','approved'));
ALTER TABLE workspaces ALTER COLUMN early_access_state SET DEFAULT 'pending';
CREATE TABLE workspace_approvals (
 workspace_id bigint PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
 operator_github_id bigint NOT NULL CHECK(operator_github_id>0),
 approved_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX workspace_early_access_pending ON workspaces(created_at,id) WHERE state='active' AND early_access_state='pending';

-- +goose Down
DROP TABLE workspace_approvals;
ALTER TABLE workspaces DROP COLUMN early_access_state;
