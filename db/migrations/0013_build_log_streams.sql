-- +goose Up
-- Current-build log streams. One row per lifetime environment describes the
-- attempt whose output is stored now. epoch increases on every admitted build
-- attempt, including a retry of the same revision, so a writer or browser cursor
-- from an earlier attempt can always be told apart from the current one. The
-- admission transaction bumps epoch and deletes the previous lines together;
-- rejected jobs leave both untouched. Lines are also deleted after retention.
CREATE TABLE build_log_streams (
    environment_id BIGINT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    epoch BIGINT NOT NULL CHECK (epoch > 0),
    generation BIGINT NOT NULL CHECK (generation >= 0),
    revision BIGINT NOT NULL CHECK (revision > 0),
    head_sha TEXT NOT NULL CHECK (head_sha <> ''),
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'finished')),
    -- Sequences are contiguous from 0, so next_sequence is also the line count.
    next_sequence BIGINT NOT NULL DEFAULT 0 CHECK (next_sequence >= 0),
    stored_bytes BIGINT NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
    -- truncated: a size or line cap was reached; omitted_lines were discarded
    -- after it. lost_lines could not be stored because storage failed.
    truncated BOOLEAN NOT NULL DEFAULT false,
    omitted_lines BIGINT NOT NULL DEFAULT 0 CHECK (omitted_lines >= 0),
    lost_lines BIGINT NOT NULL DEFAULT 0 CHECK (lost_lines >= 0),
    opened_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    finished_at TIMESTAMPTZ,
    pruned_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT build_log_streams_finished_check CHECK ((state = 'finished') = (finished_at IS NOT NULL))
);

CREATE INDEX build_log_streams_retention ON build_log_streams ((coalesce(finished_at, opened_at)))
    WHERE pruned_at IS NULL;

-- No writer existed before this migration, so no stored line needs an epoch.
ALTER TABLE build_logs
    ADD COLUMN epoch BIGINT NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'output' CHECK (kind IN ('output', 'system')),
    -- 8 KiB line cap plus a truncation marker, enforced even if code regressed.
    ADD CONSTRAINT build_logs_line_size CHECK (octet_length(line) <= 16384),
    DROP CONSTRAINT build_logs_environment_sequence_key,
    ADD CONSTRAINT build_logs_environment_epoch_sequence_key UNIQUE (environment_id, epoch, sequence);
ALTER TABLE build_logs ALTER COLUMN epoch DROP DEFAULT;

-- Every append and reset updates the stream row, so one row trigger wakes live
-- log viewers. The payload names only the environment topic; viewers read lines
-- through ownership-checked handlers. Identical payloads within a transaction
-- are delivered once.
-- +goose StatementBegin
CREATE FUNCTION previa_build_log_stream_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD IS NOT DISTINCT FROM NEW THEN
        RETURN NULL;
    END IF;
    PERFORM pg_notify('previa_events',
        json_build_object('topic', 'buildlog:' || NEW.environment_id, 'source', 'build_logs')::text);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER build_log_streams_realtime AFTER INSERT OR UPDATE ON build_log_streams
    FOR EACH ROW EXECUTE FUNCTION previa_build_log_stream_changed();

-- +goose Down
DROP TRIGGER build_log_streams_realtime ON build_log_streams;
DROP FUNCTION previa_build_log_stream_changed();
-- Admission deletes earlier attempts' lines, so (environment, sequence) is unique.
ALTER TABLE build_logs
    DROP CONSTRAINT build_logs_environment_epoch_sequence_key,
    ADD CONSTRAINT build_logs_environment_sequence_key UNIQUE (environment_id, sequence),
    DROP CONSTRAINT build_logs_line_size,
    DROP COLUMN kind,
    DROP COLUMN epoch;
DROP TABLE build_log_streams;
