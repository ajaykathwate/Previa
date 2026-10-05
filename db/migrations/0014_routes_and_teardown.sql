-- +goose Up
-- Route controller state (docs/11-routing-and-https.md). One row per lifetime
-- environment holds the route version the controller accepted, ordered by
-- (generation, revision). A removal stays as a tombstone, so a late activation
-- can never bring a removed route back. The served columns record the last
-- version the router was confirmed to have loaded; cleanup keeps both targets.
CREATE TABLE environment_routes (
    environment_id BIGINT PRIMARY KEY REFERENCES environments (id),
    pull_request_id BIGINT NOT NULL UNIQUE REFERENCES pull_requests (id),
    generation BIGINT NOT NULL CHECK (generation >= 0),
    revision BIGINT NOT NULL CHECK (revision >= 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'removed')),
    container_id TEXT,
    image_tag TEXT,
    -- The container name the router dials on the PR's network, and its port.
    backend TEXT,
    port INTEGER CHECK (port BETWEEN 1 AND 65535),
    network_id TEXT,
    table_version BIGINT NOT NULL CHECK (table_version > 0),
    operation_id BIGINT,
    accepted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    served_generation BIGINT,
    served_revision BIGINT,
    served_container_id TEXT,
    served_image_tag TEXT,
    served_at TIMESTAMPTZ,
    -- Targets accepted after the last confirmed version: the router may have
    -- loaded any of them, so cleanup keeps them until a newer one is served.
    unconfirmed_targets JSONB NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(unconfirmed_targets) = 'array'),
    CONSTRAINT environment_routes_target CHECK (
        (state = 'active' AND container_id IS NOT NULL AND image_tag IS NOT NULL
            AND backend IS NOT NULL AND port IS NOT NULL AND network_id IS NOT NULL)
        OR (state = 'removed' AND container_id IS NULL AND image_tag IS NULL
            AND backend IS NULL AND port IS NULL AND network_id IS NULL)
    ),
    CONSTRAINT environment_routes_served CHECK (
        (served_generation IS NULL) = (served_revision IS NULL)
    )
);

-- One counter serializes route acceptances in commit order, so a snapshot's
-- version always covers every acceptance committed before it.
CREATE TABLE route_table (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    version BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0)
);
INSERT INTO route_table (singleton, version) VALUES (true, 0);

-- A local teardown of a PR that GitHub may still show open: access revocation
-- now, an owner's request and expiry later. Requesting one advances the PR's
-- generation, which fences every earlier build, publication and report.
ALTER TABLE pull_requests
    ADD COLUMN teardown_requested_generation BIGINT NOT NULL DEFAULT -1
        CHECK (teardown_requested_generation >= -1),
    ADD COLUMN teardown_reason TEXT
        CHECK (teardown_reason IN ('revoked', 'requested', 'expired'));

CREATE INDEX pull_requests_unstarted_teardown ON pull_requests (id)
    WHERE teardown_requested_generation = generation
      AND teardown_completed_generation < generation;

-- teardown_generation names the teardown an environment is in, so a late
-- route removal never finalizes a newer teardown. Hostname labels are DNS-safe
-- and allocated once per lifetime environment; they are never reassigned.
ALTER TABLE environments
    ADD COLUMN teardown_generation BIGINT CHECK (teardown_generation >= 0),
    ADD CONSTRAINT environments_subdomain_label
        CHECK (subdomain ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$');

-- Environments are now provisioned when a build is first admitted. Expiry is
-- scheduled by housekeeping; until it is, an environment has no expiry.
ALTER TABLE environments ALTER COLUMN expires_at DROP NOT NULL;

-- +goose Down
-- Restoring NOT NULL fails while unscheduled environments exist, rather than
-- inventing expiry times during a rollback.
ALTER TABLE environments ALTER COLUMN expires_at SET NOT NULL;
ALTER TABLE environments
    DROP CONSTRAINT environments_subdomain_label,
    DROP COLUMN teardown_generation;
DROP INDEX pull_requests_unstarted_teardown;
ALTER TABLE pull_requests
    DROP COLUMN teardown_reason,
    DROP COLUMN teardown_requested_generation;
DROP TABLE route_table;
DROP TABLE environment_routes;
