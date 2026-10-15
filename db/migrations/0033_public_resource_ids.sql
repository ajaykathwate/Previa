-- +goose Up
-- Public addresses are random UUIDs. Existing keys and foreign-key relationships
-- remain stable for workers, accepted routes, accounting and historical jobs.
ALTER TABLE users ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;
ALTER TABLE workspaces ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;
ALTER TABLE installations ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;
ALTER TABLE repos ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;
ALTER TABLE pull_requests ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;
ALTER TABLE workspace_invitations ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;
ALTER TABLE repository_destinations ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;
ALTER TABLE environments ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;

-- +goose Down
ALTER TABLE environments DROP COLUMN public_id;
ALTER TABLE repository_destinations DROP COLUMN public_id;
ALTER TABLE workspace_invitations DROP COLUMN public_id;
ALTER TABLE pull_requests DROP COLUMN public_id;
ALTER TABLE repos DROP COLUMN public_id;
ALTER TABLE installations DROP COLUMN public_id;
ALTER TABLE workspaces DROP COLUMN public_id;
ALTER TABLE users DROP COLUMN public_id;
