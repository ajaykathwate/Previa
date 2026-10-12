-- +goose Up
-- What happens to a live preview after it is published
-- (docs/10-preview-runtime.md).

-- A container of a live preview stopped. One row per Docker "die" event,
-- keyed by the event's time, so workers sharing a Docker host record (and
-- copy the output of) each crash once.
CREATE TABLE preview_crashes (
    container_id TEXT NOT NULL CHECK (container_id ~ '^[0-9a-f]{64}$'),
    event_nanos BIGINT NOT NULL CHECK (event_nanos > 0),
    environment_id BIGINT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    exit_code INTEGER NOT NULL,
    crashed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (container_id, event_nanos)
);
CREATE INDEX preview_crashes_environment ON preview_crashes (environment_id, crashed_at);

-- Requests counted by the router: how many reached the preview, and when the
-- last one did (to the minute). No request details are kept.
ALTER TABLE environments
    ADD COLUMN visits BIGINT NOT NULL DEFAULT 0 CHECK (visits >= 0),
    ADD COLUMN last_visited_at TIMESTAMPTZ;

-- The router's request counter of each preview's service at the last read,
-- so each read adds only what is new. A new service (another version) or a
-- smaller counter (the router restarted) counts from zero.
CREATE TABLE preview_visit_counters (
    environment_id BIGINT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    service TEXT NOT NULL CHECK (service <> ''),
    requests DOUBLE PRECISION NOT NULL CHECK (requests >= 0),
    read_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- +goose Down
DROP TABLE preview_visit_counters;
ALTER TABLE environments DROP COLUMN last_visited_at, DROP COLUMN visits;
DROP TABLE preview_crashes;
