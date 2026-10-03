-- +goose Up
ALTER TABLE pull_requests ADD COLUMN teardown_completed_generation BIGINT NOT NULL DEFAULT -1 CHECK (teardown_completed_generation >= -1);
ALTER TABLE environments ADD COLUMN applied_revision BIGINT NOT NULL DEFAULT 0 CHECK (applied_revision >= 0),
    ADD COLUMN pending_revision BIGINT CHECK (pending_revision >= 0);
ALTER TABLE environments DROP CONSTRAINT environments_pending_generation_check;
ALTER TABLE environments ADD CONSTRAINT environments_pending_generation_check CHECK (pending_generation >= 0);
ALTER TABLE environment_operations DROP CONSTRAINT environment_operations_generation_check;
ALTER TABLE environment_operations ADD CONSTRAINT environment_operations_generation_check CHECK (generation >= 0);
ALTER TABLE environment_operations ADD COLUMN revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0);

-- +goose Down
-- Refuse rollback when generation-zero publication data exists rather than deleting it.
ALTER TABLE environment_operations DROP COLUMN revision;
ALTER TABLE environment_operations DROP CONSTRAINT environment_operations_generation_check;
ALTER TABLE environment_operations ADD CONSTRAINT environment_operations_generation_check CHECK (generation > 0);
ALTER TABLE environments DROP CONSTRAINT environments_pending_generation_check;
ALTER TABLE environments ADD CONSTRAINT environments_pending_generation_check CHECK (pending_generation > 0);
ALTER TABLE environments DROP COLUMN pending_revision, DROP COLUMN applied_revision;
ALTER TABLE pull_requests DROP COLUMN teardown_completed_generation;
