-- +goose Up
ALTER TABLE pull_requests ADD COLUMN description TEXT NOT NULL DEFAULT '', ADD COLUMN description_updated_at TIMESTAMPTZ;
-- Keep the execution address separate from domain identity. River can clean up
-- old jobs, so this is intentionally not a foreign key into River's own schema.
ALTER TABLE build_intents ADD COLUMN execution_job_id BIGINT,
 ADD COLUMN execution_error_code TEXT NOT NULL DEFAULT '';

-- Publish retries change no environment row yet. Notify the same authorized
-- repository stream so the UI can replace its spinner with a retry explanation.
-- +goose StatementBegin
CREATE FUNCTION previa_publication_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE repo BIGINT;
BEGIN
 SELECT repo_id INTO repo FROM pull_requests WHERE id=NEW.pull_request_id;
 PERFORM previa_notify_repository(repo,'publication');
 RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER environment_operations_publication_realtime AFTER UPDATE OF state,last_error_code ON environment_operations
FOR EACH ROW WHEN (NEW.kind='activate_route' AND (OLD.state IS DISTINCT FROM NEW.state OR OLD.last_error_code IS DISTINCT FROM NEW.last_error_code))
EXECUTE FUNCTION previa_publication_changed();

-- +goose Down
DROP TRIGGER environment_operations_publication_realtime ON environment_operations;
DROP FUNCTION previa_publication_changed();

ALTER TABLE build_intents DROP COLUMN execution_error_code, DROP COLUMN execution_job_id;
ALTER TABLE pull_requests DROP COLUMN description_updated_at, DROP COLUMN description;
