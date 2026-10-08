-- +goose Up
-- GitHub reporting state (docs/14-pr-feedback.md). One row per reported PR.
-- Every reportable change bumps wanted_version in its own transaction together
-- with a durable report operation; the reporter renders the PR's current state
-- and records the version it wrote. The living comment's ID stays in
-- pull_requests.github_comment_id. The current check run is identified by the
-- commit and request revision it describes.
CREATE TABLE pull_request_reports (
    pull_request_id BIGINT PRIMARY KEY REFERENCES pull_requests (id),
    wanted_version BIGINT NOT NULL DEFAULT 0 CHECK (wanted_version >= 0),
    reported_version BIGINT NOT NULL DEFAULT 0 CHECK (reported_version >= 0),
    comment_digest TEXT,
    check_head_sha TEXT,
    check_revision BIGINT CHECK (check_revision >= 0),
    check_run_id BIGINT CHECK (check_run_id > 0),
    check_open BOOLEAN NOT NULL DEFAULT false,
    check_digest TEXT,
    -- One reporter at a time per PR, so a first report never creates two
    -- comments; a lease outlives a crashed reporter only until it expires.
    lease_owner TEXT,
    lease_expires_at TIMESTAMPTZ,
    last_error_code TEXT CHECK (last_error_code ~ '^[a-z0-9_]{1,64}$'),
    reported_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT pull_request_reports_lease CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CONSTRAINT pull_request_reports_check CHECK (
        (check_run_id IS NULL AND check_head_sha IS NULL AND check_revision IS NULL)
        OR (check_head_sha IS NOT NULL AND check_revision IS NOT NULL)
    )
);

CREATE INDEX pull_request_reports_behind ON pull_request_reports (pull_request_id)
    WHERE reported_version < wanted_version;

-- The safe failure category of the environment's latest failed attempt, for
-- reports and the dashboard. Never a raw error message.
ALTER TABLE environments
    ADD COLUMN last_error_code TEXT CHECK (last_error_code ~ '^[a-z0-9_]{1,64}$');

-- +goose Down
ALTER TABLE environments DROP COLUMN last_error_code;
DROP INDEX pull_request_reports_behind;
DROP TABLE pull_request_reports;
