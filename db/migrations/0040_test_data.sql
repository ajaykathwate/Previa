-- +goose Up
-- Ready-to-test sample data (README-MVP-PRODUCT-EXPANSION.md, Feature 5).
-- Seeding itself stays the repository's own `setup` command, run once per
-- candidate; Previa records what happened and keeps shared test-login
-- material for reviewers.

-- How long a candidate's setup command ran. Recorded with the candidate's
-- components once it is ready, so a deployment says "Setup completed in 12 s"
-- and never inherits the outcome of another candidate. NULL: no setup
-- declared, or recorded before this migration.
ALTER TABLE preview_components
    ADD COLUMN setup_ms INTEGER CHECK (setup_ms IS NULL OR setup_ms >= 0);

-- A repository's test access: how reviewers sign in to its previews. The
-- account text (an email, a username, a hint) is shown to everyone who may
-- open the review; the secret (a password or token for a preview-only
-- account the repository's seed creates) is sealed with the platform's
-- secret keys, bound to the repository, written only by Owners and Admins
-- and revealed on request to members with preview access, each reveal
-- recorded. It is never part of a page, a stream, GitHub or activity text.
CREATE TABLE review_test_access (
    repo_id BIGINT PRIMARY KEY REFERENCES repos (id) ON DELETE CASCADE,
    id BIGINT GENERATED ALWAYS AS IDENTITY NOT NULL UNIQUE,
    account TEXT NOT NULL DEFAULT '' CHECK (length(account) <= 300),
    secret_ciphertext BYTEA CHECK (secret_ciphertext IS NULL OR octet_length(secret_ciphertext) <= 1024),
    key_version INTEGER CHECK (key_version IS NULL OR key_version > 0),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_by BIGINT NOT NULL REFERENCES users (id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    last_revealed_by BIGINT REFERENCES users (id),
    last_revealed_at TIMESTAMPTZ,
    CHECK ((secret_ciphertext IS NULL) = (key_version IS NULL)),
    CHECK (account <> '' OR secret_ciphertext IS NOT NULL)
);

-- Saves wake the repository's live pages; the payload never carries data. A
-- reveal or a key rotation keeps the revision: nothing shown live changes
-- (the last reveal appears on the next load), and a refresh would hide the
-- value someone just revealed.
-- +goose StatementBegin
CREATE FUNCTION previa_test_access_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.revision = NEW.revision THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' THEN
        PERFORM previa_notify_repository(OLD.repo_id, TG_TABLE_NAME);
    ELSE
        PERFORM previa_notify_repository(NEW.repo_id, TG_TABLE_NAME);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER review_test_access_realtime AFTER INSERT OR UPDATE OR DELETE ON review_test_access
    FOR EACH ROW EXECUTE FUNCTION previa_test_access_changed();

-- +goose Down
DROP TRIGGER review_test_access_realtime ON review_test_access;
DROP FUNCTION previa_test_access_changed();
DROP TABLE review_test_access;
ALTER TABLE preview_components DROP COLUMN setup_ms;
