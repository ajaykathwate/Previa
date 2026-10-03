-- +goose Up
ALTER TABLE repos ADD COLUMN build_mode TEXT NOT NULL DEFAULT 'manual'
    CHECK (build_mode IN ('manual', 'automatic'));

-- One current coordination record per PR, not historical build attempts.
-- Generation describes observed PR state; revision describes authorized work.
CREATE TABLE build_intents (
    pull_request_id BIGINT PRIMARY KEY REFERENCES pull_requests(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL CHECK (revision > 0),
    base_revision BIGINT NOT NULL CHECK (base_revision >= 0),
    generation BIGINT NOT NULL CHECK (generation >= 0),
    head_sha TEXT NOT NULL CHECK (head_sha <> ''),
    source TEXT NOT NULL CHECK (source IN ('manual', 'automatic')),
    request_action TEXT NOT NULL CHECK (request_action IN ('build', 'retry', 'rebuild', 'automatic')),
    requested_by BIGINT REFERENCES users(id),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    state TEXT NOT NULL CHECK (state IN ('pending', 'queued', 'running', 'succeeded', 'failed', 'cancelled', 'superseded')),
    available_at TIMESTAMPTZ NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT build_intents_revision_successor_check CHECK (revision = base_revision + 1),
    CONSTRAINT build_intents_actor_check CHECK (
        (source = 'manual' AND requested_by IS NOT NULL AND request_action <> 'automatic')
        OR (source = 'automatic' AND requested_by IS NULL AND request_action = 'automatic')
    ),
    CONSTRAINT build_intents_completion_check CHECK (
        (state IN ('pending', 'queued', 'running') AND completed_at IS NULL)
        OR (state IN ('succeeded', 'failed', 'cancelled', 'superseded') AND completed_at IS NOT NULL)
    ),
    CONSTRAINT build_intents_started_check CHECK (
        (state IN ('pending', 'queued') AND started_at IS NULL)
        OR (state IN ('running', 'succeeded', 'failed') AND started_at IS NOT NULL)
        OR state IN ('cancelled', 'superseded')
    )
);

CREATE INDEX build_intents_due ON build_intents(available_at, pull_request_id)
    WHERE state = 'pending';

-- +goose Down
DROP TABLE build_intents;
ALTER TABLE repos DROP COLUMN build_mode;
