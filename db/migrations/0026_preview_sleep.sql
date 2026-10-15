-- +goose Up
-- Idle previews sleep (docs/10-preview-runtime.md): after PREVIEW_IDLE_SLEEP
-- without a visit, the worker stops a preview's app and processes and the
-- router sends its visitors to a wake page; the next visit starts it again.
-- The environment stays "running" (it keeps its address, route and services);
-- asleep_at marks the sleep.
ALTER TABLE environments
    ADD COLUMN asleep_at TIMESTAMPTZ,
    ADD COLUMN woke_at TIMESTAMPTZ,
    -- A visitor is waiting for it to wake; wake_error says why it could not.
    ADD COLUMN wake_requested_at TIMESTAMPTZ,
    ADD COLUMN wake_error TEXT NOT NULL DEFAULT '' CHECK (length(wake_error) <= 500);

-- +goose Down
ALTER TABLE environments
    DROP COLUMN wake_error, DROP COLUMN wake_requested_at,
    DROP COLUMN woke_at, DROP COLUMN asleep_at;
