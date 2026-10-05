-- +goose Up
-- What each running worker process can do, refreshed by a heartbeat
-- (docs/21-mvp-operations.md). The server reads it to tell owners honestly
-- whether builds can run; it never decides execution itself.
CREATE TABLE worker_capabilities (
    worker_id TEXT PRIMARY KEY CHECK (worker_id ~ '^[a-z0-9]{16,64}$'),
    execution BOOLEAN NOT NULL,
    routing BOOLEAN NOT NULL,
    reporting BOOLEAN NOT NULL,
    housekeeping BOOLEAN NOT NULL,
    discovery BOOLEAN NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX worker_capabilities_seen ON worker_capabilities (seen_at);

-- +goose Down
DROP TABLE worker_capabilities;
