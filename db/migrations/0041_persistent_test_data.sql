-- +goose Up
-- Persistent test data (README-MVP-PRODUCT-EXPANSION.md, Feature 5B;
-- docs/32-persistent-test-data.md). Data is shared as copies, never as a live
-- database: a repository baseline built from one branch, and a pull
-- request's own serving data copied into its next candidate.

-- A repository's Test data settings: Owners and Admins only, never repository
-- YAML. No row means Test data is off.
CREATE TABLE repository_sampledata_settings (
    repo_id BIGINT PRIMARY KEY REFERENCES repos (id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT false,
    keep_data BOOLEAN NOT NULL DEFAULT true,
    use_baseline BOOLEAN NOT NULL DEFAULT true,
    -- NULL: the repository's GitHub default branch, read at every refresh.
    baseline_branch TEXT CHECK (baseline_branch IS NULL OR (length(baseline_branch) BETWEEN 1 AND 255
        AND baseline_branch !~ '[[:cntrl:][:space:]~^:?*\\[]' AND baseline_branch !~ '\.\.')),
    auto_refresh BOOLEAN NOT NULL DEFAULT true,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_by BIGINT NOT NULL REFERENCES users (id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- The latest look at the baseline branch's head (a refresh or the daily
    -- freshness check): the tab says when the branch moved past the baseline.
    checked_at TIMESTAMPTZ,
    checked_branch TEXT CHECK (checked_branch IS NULL OR length(checked_branch) BETWEEN 1 AND 255),
    checked_sha TEXT CHECK (checked_sha IS NULL OR checked_sha ~ '^[0-9a-f]{40}$')
);

-- One row per baseline refresh. Requests made while one waits collapse into
-- it; one made while another runs becomes the single follow-up. sequence
-- orders refreshes: a finished refresh becomes current only if it is newer
-- than the current baseline.
CREATE TABLE baseline_refreshes (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id BIGINT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    trigger TEXT NOT NULL CHECK (trigger IN ('enabled', 'merged', 'button', 'daily', 'unusable', 'branch')),
    requested_by BIGINT REFERENCES users (id) ON DELETE SET NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    -- Automatic refreshes of one repository are spaced apart.
    available_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- Why an admitted refresh waits (it keeps its place).
    waiting_reason TEXT CHECK (waiting_reason IN ('capacity_wait', 'quota_blocked')),
    branch TEXT CHECK (branch IS NULL OR length(branch) BETWEEN 1 AND 255),
    commit_sha TEXT CHECK (commit_sha IS NULL OR commit_sha ~ '^[0-9a-f]{40}$'),
    failure_code TEXT CHECK (failure_code IN ('branch_not_found', 'config_invalid', 'build_failed', 'service_failed', 'setup_failed',
        'seed_failed', 'capture_failed', 'capture_too_large', 'quota_blocked', 'capacity_wait', 'unavailable')),
    -- A safe sentence for the Test data tab, such as what to add to the YAML.
    failure_detail TEXT CHECK (failure_detail IS NULL OR length(failure_detail) <= 500),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    build_seconds INTEGER CHECK (build_seconds IS NULL OR build_seconds >= 0),
    -- The refresh's build log (lines in baseline_logs), bounded like a pull
    -- request attempt's; log_open is true while its writer may append.
    log_open BOOLEAN NOT NULL DEFAULT false,
    log_lines BIGINT NOT NULL DEFAULT 0 CHECK (log_lines >= 0),
    log_bytes BIGINT NOT NULL DEFAULT 0 CHECK (log_bytes >= 0),
    log_truncated BOOLEAN NOT NULL DEFAULT false,
    log_omitted BIGINT NOT NULL DEFAULT 0 CHECK (log_omitted >= 0),
    log_lost BIGINT NOT NULL DEFAULT 0 CHECK (log_lost >= 0),
    requested_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (repo_id, sequence),
    CHECK ((state IN ('succeeded', 'failed', 'cancelled')) = (finished_at IS NOT NULL))
);
CREATE UNIQUE INDEX baseline_refreshes_one_pending ON baseline_refreshes (repo_id) WHERE state = 'pending';
CREATE UNIQUE INDEX baseline_refreshes_one_running ON baseline_refreshes (repo_id) WHERE state = 'running';

-- A baseline refresh's build log: the same line kinds and size cap as a pull
-- request attempt's (build_logs).
CREATE TABLE baseline_logs (
    refresh_id BIGINT NOT NULL REFERENCES baseline_refreshes (id) ON DELETE CASCADE,
    sequence BIGINT NOT NULL CHECK (sequence >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('output', 'system')),
    line TEXT NOT NULL CHECK (octet_length(line) <= 16384),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (refresh_id, sequence)
);

-- A repository's baselines: one encrypted dump per SQL service. The file is
-- on the worker (PREVIEW_BASELINE_DIR); this row is its metadata, never its
-- rows. At most one is current per service kind.
CREATE TABLE repository_baselines (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id BIGINT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    refresh_id BIGINT REFERENCES baseline_refreshes (id) ON DELETE SET NULL,
    service_kind TEXT NOT NULL CHECK (service_kind IN ('postgres', 'mysql')),
    service_version TEXT NOT NULL CHECK (service_version ~ '^[0-9]{1,3}(\.[0-9]{1,3}){0,2}$'),
    branch TEXT NOT NULL CHECK (length(branch) BETWEEN 1 AND 255),
    commit_sha TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    config_hash TEXT,
    refresh_sequence BIGINT NOT NULL CHECK (refresh_sequence > 0),
    storage_key TEXT NOT NULL UNIQUE CHECK (storage_key ~ '^[a-z0-9-]{8,128}$'),
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    checksum TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    key_version INTEGER NOT NULL CHECK (key_version > 0),
    status TEXT NOT NULL CHECK (status IN ('current', 'retired', 'missing')),
    built_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    retired_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX repository_baselines_current ON repository_baselines (repo_id, service_kind) WHERE status = 'current';
CREATE INDEX repository_baselines_retired ON repository_baselines (retired_at) WHERE status <> 'current';

-- Where a candidate's databases started (docs/32): kept from the serving
-- candidate, the repository baseline or empty, with every fallback and its
-- reason, and whether the seed command ran. data_source is NULL without a
-- SQL service.
ALTER TABLE preview_components
    ADD COLUMN seed BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN seed_ms INTEGER CHECK (seed_ms IS NULL OR seed_ms >= 0),
    ADD COLUMN data_source TEXT CHECK (data_source IN ('kept', 'baseline', 'empty')),
    ADD COLUMN data_detail JSONB NOT NULL DEFAULT '{}'
        CHECK (jsonb_typeof(data_detail) = 'object' AND octet_length(data_detail::text) <= 4096);

-- A Reset test data request's chosen starting source; NULL for every other
-- request, which follows the repository's settings.
ALTER TABLE build_intents
    ADD COLUMN start_source TEXT CHECK (start_source IS NULL OR start_source IN ('baseline', 'empty'));

-- Execution workers announce whether they can store baselines (they hold
-- the secret keys that encrypt them), so the Test data tab can say why
-- refreshes wait.
ALTER TABLE worker_capabilities ADD COLUMN baselines BOOLEAN NOT NULL DEFAULT false;

-- Changes wake the repository's live pages (the Test data tab, pull request
-- pages); payloads never carry data. A refresh's log counters change with
-- every stored line, so only its state, waiting reason and result notify.
-- +goose StatementBegin
CREATE FUNCTION previa_test_data_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Nested: a record's fields are resolved only for the table that has them.
    IF TG_OP = 'UPDATE' AND TG_TABLE_NAME = 'baseline_refreshes' THEN
        IF (OLD.state, OLD.waiting_reason, OLD.branch, OLD.commit_sha, OLD.failure_code, OLD.available_at)
           IS NOT DISTINCT FROM (NEW.state, NEW.waiting_reason, NEW.branch, NEW.commit_sha, NEW.failure_code, NEW.available_at) THEN
            RETURN NULL;
        END IF;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD IS NOT DISTINCT FROM NEW THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' THEN
        PERFORM previa_notify_repository(OLD.repo_id, TG_TABLE_NAME);
    ELSE
        PERFORM previa_notify_repository(NEW.repo_id, TG_TABLE_NAME);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER repository_sampledata_settings_realtime AFTER INSERT OR UPDATE OR DELETE ON repository_sampledata_settings
    FOR EACH ROW EXECUTE FUNCTION previa_test_data_changed();
CREATE TRIGGER baseline_refreshes_realtime AFTER INSERT OR UPDATE OR DELETE ON baseline_refreshes
    FOR EACH ROW EXECUTE FUNCTION previa_test_data_changed();
CREATE TRIGGER repository_baselines_realtime AFTER INSERT OR UPDATE OR DELETE ON repository_baselines
    FOR EACH ROW EXECUTE FUNCTION previa_test_data_changed();

-- +goose Down
DROP TRIGGER repository_baselines_realtime ON repository_baselines;
DROP TRIGGER baseline_refreshes_realtime ON baseline_refreshes;
DROP TRIGGER repository_sampledata_settings_realtime ON repository_sampledata_settings;
DROP FUNCTION previa_test_data_changed();
ALTER TABLE worker_capabilities DROP COLUMN baselines;
ALTER TABLE build_intents DROP COLUMN start_source;
ALTER TABLE preview_components
    DROP COLUMN data_detail,
    DROP COLUMN data_source,
    DROP COLUMN seed_ms,
    DROP COLUMN seed;
DROP TABLE repository_baselines;
DROP TABLE baseline_logs;
DROP TABLE baseline_refreshes;
DROP TABLE repository_sampledata_settings;
