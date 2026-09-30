-- +goose Up
ALTER TABLE users ADD COLUMN avatar_url TEXT NOT NULL DEFAULT '';

-- SCS pgxstore requires this table; it does not run migrations itself.
CREATE TABLE sessions (
    token TEXT PRIMARY KEY,
    data BYTEA NOT NULL,
    expiry TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- +goose Down
DROP TABLE sessions;
ALTER TABLE users DROP COLUMN avatar_url;
