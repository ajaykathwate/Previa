-- +goose Up
ALTER TABLE environments
    DROP CONSTRAINT environments_status_check;

ALTER TABLE environments
    ADD CONSTRAINT environments_status_check CHECK (
        status IN ('queued', 'building', 'publishing', 'running', 'failed', 'tearing_down', 'torn_down')
    );

ALTER TABLE environments
    ADD COLUMN pending_container_id TEXT,
    ADD COLUMN pending_image_tag TEXT,
    ADD COLUMN pending_generation BIGINT CHECK (pending_generation > 0),
    ADD COLUMN pending_head_sha TEXT;

CREATE TABLE environment_operations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_request_id BIGINT NOT NULL REFERENCES pull_requests (id),
    generation BIGINT NOT NULL CHECK (generation > 0),
    kind TEXT NOT NULL CHECK (kind IN (
        'activate_route', 'remove_route', 'report_status', 'cleanup_artifact'
    )),
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    payload JSONB NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending', 'running', 'retryable', 'succeeded', 'superseded', 'failed'
    )),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    last_error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    CONSTRAINT environment_operations_lease_check CHECK (
        (lease_owner IS NULL AND lease_expires_at IS NULL)
        OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
    )
);

CREATE INDEX environment_operations_due
    ON environment_operations (available_at, id)
    WHERE state IN ('pending', 'retryable');

CREATE INDEX environment_operations_pr_generation
    ON environment_operations (pull_request_id, generation, id);

-- +goose Down
DROP INDEX environment_operations_pr_generation;
DROP INDEX environment_operations_due;
DROP TABLE environment_operations;
ALTER TABLE environments
    DROP COLUMN pending_head_sha,
    DROP COLUMN pending_generation,
    DROP COLUMN pending_image_tag,
    DROP COLUMN pending_container_id;
ALTER TABLE environments
    DROP CONSTRAINT environments_status_check;
UPDATE environments SET status = 'failed' WHERE status = 'publishing';
ALTER TABLE environments
    ADD CONSTRAINT environments_status_check CHECK (
        status IN ('queued', 'building', 'running', 'failed', 'tearing_down', 'torn_down')
    );
