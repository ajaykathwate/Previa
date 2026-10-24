-- +goose Up
-- One .env file per repository: the text its Owners and Admins edit, with
-- comments and order kept. encrypted_text is nonce || AES-256-GCM ciphertext
-- like env_secrets values, authenticated as the repository's ".env". The
-- secrets service keeps env_secrets (what previews, builds and log redaction
-- read) equal to the file's names with values, in the same transaction.
CREATE TABLE repository_env_files (
    repo_id BIGINT PRIMARY KEY REFERENCES repos (id) ON DELETE CASCADE,
    -- 12-byte nonce + 0..65536 text bytes + 16-byte tag.
    encrypted_text BYTEA NOT NULL CHECK (octet_length(encrypted_text) BETWEEN 28 AND 65564),
    key_version INTEGER NOT NULL CHECK (key_version > 0),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_by BIGINT REFERENCES users (id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- Re-encryption walks files that are not on the active key version.
CREATE INDEX repository_env_files_key_version ON repository_env_files (key_version, repo_id);

-- +goose Down
DROP TABLE repository_env_files;
