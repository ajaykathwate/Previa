-- +goose Up
ALTER TABLE pull_requests ADD COLUMN generation BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0);
ALTER TABLE environments ADD COLUMN applied_generation BIGINT NOT NULL DEFAULT 0 CHECK (applied_generation >= 0);
ALTER TABLE environments ADD COLUMN head_sha TEXT;

-- +goose Down
ALTER TABLE environments DROP COLUMN head_sha;
ALTER TABLE environments DROP COLUMN applied_generation;
ALTER TABLE pull_requests DROP COLUMN generation;
