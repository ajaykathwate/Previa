-- +goose Up
-- Every live routing worker must be configured for private-preview checks.
-- Older workers default false during a rolling upgrade.
ALTER TABLE worker_capabilities ADD COLUMN private_previews BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE worker_capabilities DROP COLUMN private_previews;
