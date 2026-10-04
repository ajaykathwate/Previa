-- +goose Up
ALTER TABLE pull_requests
    ADD COLUMN mergeable boolean,
    ADD COLUMN mergeable_state text NOT NULL DEFAULT '',
    ADD COLUMN build_status text NOT NULL DEFAULT 'checking'
        CHECK (build_status IN ('open','checking','conflicts'));

-- Existing observations have no verified mergeability; fail closed until refreshed.

-- +goose Down
ALTER TABLE pull_requests DROP COLUMN build_status, DROP COLUMN mergeable_state,
    DROP COLUMN mergeable;
