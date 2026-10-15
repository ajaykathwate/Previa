-- +goose Up
-- A removed member stays visible as deactivated. Who removed them and the
-- repositories they had are kept so a re-invitation can offer the same access.
ALTER TABLE workspace_memberships
 ADD COLUMN removed_by BIGINT REFERENCES users(id),
 ADD COLUMN removed_repository_ids BIGINT[] NOT NULL DEFAULT '{}';

-- Invited people see their pending invitations in Previa, by GitHub identity.
CREATE INDEX workspace_invitations_recipient ON workspace_invitations(github_id) WHERE state='pending';

-- Member activity lists a person's own build requests, newest first.
CREATE INDEX build_intents_requested_by ON build_intents(requested_by, requested_at DESC) WHERE requested_by IS NOT NULL;

-- Deleting a workspace is a soft delete: access functions already require 'active'.
ALTER TABLE workspaces ADD COLUMN deleted_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE workspaces DROP COLUMN deleted_at;
DROP INDEX build_intents_requested_by;
DROP INDEX workspace_invitations_recipient;
ALTER TABLE workspace_memberships DROP COLUMN removed_repository_ids, DROP COLUMN removed_by;
