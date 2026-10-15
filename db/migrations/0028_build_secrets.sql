-- +goose Up
-- Build secrets (docs/07-repository-secrets.md): an owner may let Dockerfile
-- steps read a repository secret (RUN --mount=type=secret), for private
-- package registries and base images. Never for forks or unverified heads.
ALTER TABLE env_secrets ADD COLUMN build BOOLEAN NOT NULL DEFAULT false;
-- Starter kits validate edited build timeouts against live worker ceilings.
ALTER TABLE worker_capabilities ADD COLUMN build_timeout_ns BIGINT NOT NULL DEFAULT 720000000000
    CHECK (build_timeout_ns > 0 AND build_timeout_ns <= 86400000000000);

-- +goose Down
ALTER TABLE env_secrets DROP COLUMN build;
ALTER TABLE worker_capabilities DROP COLUMN build_timeout_ns;
