-- +goose Up
-- Backend previews (docs/11-backend-previews.md).

-- What a candidate ran next to its app when it became ready: its managed
-- services, whether a setup command ran, its processes, its docs path and
-- how many approved outbound destinations it was given. The dashboard and
-- the PR comment describe the deployed preview from the row of its
-- (generation, revision). It never holds credentials.
CREATE TABLE preview_components (
    pull_request_id BIGINT NOT NULL REFERENCES pull_requests (id) ON DELETE CASCADE,
    generation BIGINT NOT NULL CHECK (generation >= 0),
    revision BIGINT NOT NULL CHECK (revision >= 0),
    services JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(services) = 'array'),
    setup BOOLEAN NOT NULL DEFAULT false,
    processes JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(processes) = 'array'),
    docs_path TEXT CHECK (docs_path IS NULL OR docs_path ~ '^/'),
    outbound INTEGER NOT NULL DEFAULT 0 CHECK (outbound >= 0),
    -- The repository approves destinations but this candidate got none,
    -- because its pull request comes from a fork.
    outbound_withheld BOOLEAN NOT NULL DEFAULT false,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (pull_request_id, generation, revision)
);

-- Outbound connections a repository's owner approved for its previews (an
-- external database or message broker): one DNS name per port. Never set by
-- repository YAML; the egress proxy forwards only to these.
CREATE TABLE repository_destinations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id BIGINT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    host TEXT NOT NULL CHECK (length(host) <= 253
        AND host ~ '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$'),
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    created_by BIGINT REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo_id, port)
);

-- The managed services a live execution worker offers (its catalog, such as
-- "postgres 17, 16"), so the dashboard can say when none are configured.
ALTER TABLE worker_capabilities ADD COLUMN services TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE worker_capabilities DROP COLUMN services;
DROP TABLE repository_destinations;
DROP TABLE preview_components;
