-- +goose Up
-- Repository secrets. encrypted_value is nonce || AES-256-GCM ciphertext, with
-- the repository, name and key version authenticated as associated data, so a
-- value cannot be moved to another repository, name or version. Nothing wrote
-- env_secrets before this migration, so key_version needs no backfill; the
-- migration fails rather than guess if unexpected rows exist.
ALTER TABLE env_secrets
    ADD COLUMN key_version INTEGER NOT NULL CHECK (key_version > 0),
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD CONSTRAINT env_secrets_key_name CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]{0,127}$'),
    -- 12-byte nonce + 1..32768 value bytes + 16-byte tag.
    ADD CONSTRAINT env_secrets_value_size CHECK (octet_length(encrypted_value) BETWEEN 29 AND 32796);

-- Re-encryption walks rows that are not on the active key version.
CREATE INDEX env_secrets_key_version ON env_secrets (key_version, id);

-- +goose Down
DROP INDEX env_secrets_key_version;
ALTER TABLE env_secrets
    DROP CONSTRAINT env_secrets_value_size,
    DROP CONSTRAINT env_secrets_key_name,
    DROP COLUMN updated_at,
    DROP COLUMN key_version;
