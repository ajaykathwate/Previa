-- +goose Up
ALTER TABLE installations ADD COLUMN reconciled_at timestamptz NOT NULL DEFAULT '-infinity';
ALTER TABLE installations ADD COLUMN reconcile_requested_at timestamptz NOT NULL DEFAULT '-infinity';
CREATE INDEX installations_reconcile_due ON installations(reconciled_at, id) WHERE status IN ('active','suspended');

-- +goose Down
DROP INDEX installations_reconcile_due;
ALTER TABLE installations DROP COLUMN reconcile_requested_at, DROP COLUMN reconciled_at;
