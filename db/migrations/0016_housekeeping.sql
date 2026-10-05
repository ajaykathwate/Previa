-- +goose Up
-- Housekeeping, expiry and capacity (docs/14-housekeeping-and-capacity.md).

-- The identity this database gives the Docker objects it creates. Every
-- object carries it as a label (and in its name), and inventory, cleanup and
-- capacity only ever consider objects with this database's identity, so a test
-- schema and a development database on one Docker host never touch each
-- other's previews even when their pull request IDs coincide.
CREATE TABLE platform_instance (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    instance TEXT NOT NULL DEFAULT substr(md5(gen_random_uuid()::text), 1, 12)
        CHECK (instance ~ '^[a-z0-9]{8,32}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO platform_instance DEFAULT VALUES;

-- A build request that cannot start for lack of capacity (all build slots or
-- the preview budget in use) or disk space. It is not a failure: the request
-- keeps its place and is retried with backoff. The revision keeps a newer
-- request from inheriting the note.
ALTER TABLE build_intents
    ADD COLUMN waiting_revision BIGINT,
    ADD COLUMN waiting_reason TEXT CHECK (waiting_reason IN ('capacity', 'disk')),
    ADD CONSTRAINT build_intents_waiting CHECK ((waiting_revision IS NULL) = (waiting_reason IS NULL));

-- When periodic reconciliation last confirmed a live PR with GitHub, so each
-- pass visits the least recently confirmed PRs first and asks GitHub about a
-- PR at most once per interval. The outcome is a safe category.
CREATE TABLE pr_reconciliations (
    pull_request_id BIGINT PRIMARY KEY REFERENCES pull_requests (id),
    reconciled_at TIMESTAMPTZ NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome ~ '^[a-z0-9_]{1,64}$')
);

-- +goose Down
DROP TABLE pr_reconciliations;
ALTER TABLE build_intents
    DROP CONSTRAINT build_intents_waiting,
    DROP COLUMN waiting_reason,
    DROP COLUMN waiting_revision;
DROP TABLE platform_instance;
