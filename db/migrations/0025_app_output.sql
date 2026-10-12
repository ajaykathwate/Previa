-- +goose Up
-- Live app output (docs/08-build-logs.md). While its owner watches, the
-- worker follows a running preview's output (redacted) into app_output_lines,
-- keeping the newest 1,000 lines. When nobody has watched for a minute it
-- stops and deletes them; housekeeping removes stopped watches an hour later.
CREATE TABLE app_output_watches (
    environment_id BIGINT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    -- Each time the worker starts following, epoch increases: viewers reset
    -- and an earlier follower's writes are fenced.
    epoch BIGINT NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    state TEXT NOT NULL DEFAULT 'requested' CHECK (state IN ('requested', 'following', 'stopped')),
    note TEXT NOT NULL DEFAULT '' CHECK (length(note) <= 500),
    next_sequence BIGINT NOT NULL DEFAULT 0 CHECK (next_sequence >= 0),
    -- seen_at: a viewer was connected; updated_at: the follower is alive.
    seen_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE app_output_lines (
    environment_id BIGINT NOT NULL REFERENCES app_output_watches (environment_id) ON DELETE CASCADE,
    epoch BIGINT NOT NULL CHECK (epoch > 0),
    sequence BIGINT NOT NULL CHECK (sequence >= 0),
    at TIMESTAMPTZ NOT NULL,
    line TEXT NOT NULL CHECK (octet_length(line) <= 16384),
    PRIMARY KEY (environment_id, epoch, sequence)
);

-- New lines and state changes wake viewers; a viewer's or follower's
-- heartbeat alone does not.
-- +goose StatementBegin
CREATE FUNCTION previa_app_output_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND (OLD.epoch, OLD.state, OLD.note, OLD.next_sequence)
        IS NOT DISTINCT FROM (NEW.epoch, NEW.state, NEW.note, NEW.next_sequence) THEN
        RETURN NULL;
    END IF;
    PERFORM pg_notify('previa_events',
        json_build_object('topic', 'appoutput:' || NEW.environment_id, 'source', 'app_output')::text);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER app_output_watches_realtime AFTER INSERT OR UPDATE ON app_output_watches
    FOR EACH ROW EXECUTE FUNCTION previa_app_output_changed();

-- +goose Down
DROP TRIGGER app_output_watches_realtime ON app_output_watches;
DROP FUNCTION previa_app_output_changed();
DROP TABLE app_output_lines;
DROP TABLE app_output_watches;
