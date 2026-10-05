-- +goose Up
-- The execution kill switch (BUILDS_PAUSED, docs/23-incident-runbook.md): a
-- paused worker keeps teardown, cleanup, routing, reports and housekeeping
-- running but starts no build. Requests wait with reason 'paused', and the
-- worker says it is paused so the dashboard can tell owners why.
ALTER TABLE build_intents DROP CONSTRAINT build_intents_waiting_reason_check;
ALTER TABLE build_intents ADD CONSTRAINT build_intents_waiting_reason_check
    CHECK (waiting_reason IN ('capacity', 'disk', 'paused'));
ALTER TABLE worker_capabilities ADD COLUMN paused BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE worker_capabilities DROP COLUMN paused;
UPDATE build_intents SET waiting_reason = NULL, waiting_revision = NULL WHERE waiting_reason = 'paused';
ALTER TABLE build_intents DROP CONSTRAINT build_intents_waiting_reason_check;
ALTER TABLE build_intents ADD CONSTRAINT build_intents_waiting_reason_check
    CHECK (waiting_reason IN ('capacity', 'disk'));
