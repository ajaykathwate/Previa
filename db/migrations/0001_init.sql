-- +goose Up
CREATE TABLE users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    github_id BIGINT NOT NULL UNIQUE CHECK (github_id > 0),
    username TEXT NOT NULL CHECK (username <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE installations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    github_installation_id BIGINT NOT NULL UNIQUE CHECK (github_installation_id > 0),
    owner_user_id BIGINT NOT NULL REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE repos (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations (id),
    full_name TEXT NOT NULL UNIQUE CHECK (full_name <> ''),
    default_config_hash TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE pull_requests (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id BIGINT NOT NULL REFERENCES repos (id),
    number INTEGER NOT NULL CHECK (number > 0),
    head_sha TEXT NOT NULL CHECK (head_sha <> ''),
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed', 'merged')),
    github_comment_id BIGINT CHECK (github_comment_id > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pull_requests_repo_number_key UNIQUE (repo_id, number)
);

CREATE TABLE environments (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pull_request_id BIGINT NOT NULL REFERENCES pull_requests (id),
    container_id TEXT,
    image_tag TEXT,
    subdomain TEXT NOT NULL UNIQUE CHECK (subdomain <> ''),
    status TEXT NOT NULL DEFAULT 'queued',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_active_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT environments_status_check CHECK (
        status IN ('queued', 'building', 'running', 'failed', 'tearing_down', 'torn_down')
    ),
    CONSTRAINT environments_expiry_check CHECK (expires_at > created_at)
);

-- Failed and tearing-down environments may still own resources. Only completed
-- teardown releases the active slot. State transitions belong in guarded updates.
CREATE UNIQUE INDEX environments_one_active_per_pr
    ON environments (pull_request_id) WHERE status <> 'torn_down';

CREATE INDEX environments_active_expiry
    ON environments (expires_at) WHERE status <> 'torn_down';

CREATE TABLE build_logs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    environment_id BIGINT NOT NULL REFERENCES environments (id),
    sequence BIGINT NOT NULL CHECK (sequence >= 0),
    line TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT build_logs_environment_sequence_key UNIQUE (environment_id, sequence)
);

CREATE TABLE env_secrets (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id BIGINT NOT NULL REFERENCES repos (id),
    key TEXT NOT NULL CHECK (key <> ''),
    encrypted_value BYTEA NOT NULL CHECK (octet_length(encrypted_value) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT env_secrets_repo_key_key UNIQUE (repo_id, key)
);

-- +goose Down
DROP TABLE env_secrets;
DROP TABLE build_logs;
DROP TABLE environments;
DROP TABLE pull_requests;
DROP TABLE repos;
DROP TABLE installations;
DROP TABLE users;
