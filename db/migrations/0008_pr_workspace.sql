-- +goose Up
CREATE TABLE webhook_deliveries (
 delivery_id TEXT PRIMARY KEY CHECK (length(delivery_id) BETWEEN 1 AND 128),
 event_type TEXT NOT NULL,
 payload_hash TEXT NOT NULL,
 received_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE installations ADD COLUMN source_updated_at TIMESTAMPTZ;
ALTER TABLE installations ADD COLUMN cleanup_pending BOOLEAN NOT NULL DEFAULT false;
CREATE TABLE installation_repository_events (
 installation_id BIGINT NOT NULL REFERENCES installations(github_installation_id),
 github_repo_id BIGINT NOT NULL,
 source_updated_at TIMESTAMPTZ NOT NULL,
 removed BOOLEAN NOT NULL,
 PRIMARY KEY(installation_id,github_repo_id)
);
ALTER TABLE repos ADD COLUMN observation_version BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pull_requests ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN author TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN head_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN html_url TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN head_repo_id BIGINT;
ALTER TABLE pull_requests ADD COLUMN is_fork BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE pull_requests ADD COLUMN source_updated_at TIMESTAMPTZ;
ALTER TABLE pull_requests ADD COLUMN needs_refresh BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX pull_requests_repo_state ON pull_requests (repo_id, state, number DESC);
CREATE TABLE pr_syncs (
 repo_id BIGINT PRIMARY KEY REFERENCES repos(id),
 revision BIGINT NOT NULL DEFAULT 1,
 state TEXT NOT NULL CHECK (state IN ('queued','syncing','complete','failed')),
 next_page INTEGER NOT NULL DEFAULT 1 CHECK (next_page > 0),
 fetched INTEGER NOT NULL DEFAULT 0,
 requested_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 completed_at TIMESTAMPTZ,
 last_success_at TIMESTAMPTZ,
 message TEXT NOT NULL DEFAULT ''
);
-- +goose Down
DROP TABLE pr_syncs;
DROP TABLE installation_repository_events;
DROP INDEX pull_requests_repo_state;
ALTER TABLE pull_requests DROP COLUMN needs_refresh, DROP COLUMN source_updated_at,
 DROP COLUMN is_fork, DROP COLUMN head_repo_id, DROP COLUMN html_url,
 DROP COLUMN head_branch, DROP COLUMN author, DROP COLUMN title;
ALTER TABLE repos DROP COLUMN observation_version;
ALTER TABLE installations DROP COLUMN cleanup_pending, DROP COLUMN source_updated_at;
DROP TABLE webhook_deliveries;
