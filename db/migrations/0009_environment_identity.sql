-- +goose Up
-- An environment is the lifetime identity of a PR, including after teardown.
-- Do not silently delete legacy duplicates or their resource/log references:
-- PostgreSQL rejects this migration atomically if duplicates need reconciliation.
ALTER TABLE environments ADD CONSTRAINT environments_pull_request_key UNIQUE (pull_request_id);
DROP INDEX environments_one_active_per_pr;

-- +goose Down
CREATE UNIQUE INDEX environments_one_active_per_pr
    ON environments (pull_request_id) WHERE status <> 'torn_down';
ALTER TABLE environments DROP CONSTRAINT environments_pull_request_key;
