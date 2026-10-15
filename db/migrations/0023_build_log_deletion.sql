-- +goose Up
-- An owner can delete a finished attempt's logs (docs/08-build-logs.md).
-- Deleting prunes the stream as retention does; deleted_at records that the
-- owner did it, so the page can say which happened.
ALTER TABLE build_log_streams ADD COLUMN deleted_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE build_log_streams DROP COLUMN deleted_at;
