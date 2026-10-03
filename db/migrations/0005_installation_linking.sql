-- +goose Up
ALTER TABLE installations ALTER COLUMN owner_user_id DROP NOT NULL;
ALTER TABLE installations ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'suspended', 'inactive'));
ALTER TABLE installations ADD COLUMN account_login TEXT NOT NULL DEFAULT '';
-- Protect a callback's API snapshot from a concurrent lifecycle event.
ALTER TABLE installations ADD COLUMN revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0);
CREATE INDEX installations_owner ON installations (owner_user_id);
ALTER TABLE repos ADD COLUMN github_repo_id BIGINT CHECK (github_repo_id > 0);
CREATE UNIQUE INDEX repos_github_repo_id ON repos (github_repo_id) WHERE github_repo_id IS NOT NULL;
ALTER TABLE repos ADD COLUMN accessible BOOLEAN NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE repos DROP COLUMN accessible;
DROP INDEX repos_github_repo_id;
ALTER TABLE repos DROP COLUMN github_repo_id;
DROP INDEX installations_owner;
ALTER TABLE installations DROP COLUMN revision;
ALTER TABLE installations DROP COLUMN account_login;
ALTER TABLE installations DROP COLUMN status;
-- Cannot restore NOT NULL while unlinked installations exist; fail safely rather
-- than deleting installation/repository history during rollback.
ALTER TABLE installations ALTER COLUMN owner_user_id SET NOT NULL;
