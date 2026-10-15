-- +goose Up
ALTER TABLE users
 ADD COLUMN first_name TEXT NOT NULL DEFAULT '' CHECK(char_length(first_name)<=80),
 ADD COLUMN last_name TEXT NOT NULL DEFAULT '' CHECK(char_length(last_name)<=80),
 ADD COLUMN profile_revision BIGINT NOT NULL DEFAULT 1 CHECK(profile_revision>0),
 ADD COLUMN github_name TEXT NOT NULL DEFAULT '',
 ADD COLUMN email TEXT NOT NULL DEFAULT '',
 ADD COLUMN email_sync_error BOOLEAN NOT NULL DEFAULT false,
 ADD COLUMN github_profile_url TEXT NOT NULL DEFAULT '',
 ADD COLUMN github_bio TEXT NOT NULL DEFAULT '',
 ADD COLUMN github_company TEXT NOT NULL DEFAULT '',
 ADD COLUMN github_location TEXT NOT NULL DEFAULT '',
 ADD COLUMN github_created_at TIMESTAMPTZ,
 ADD COLUMN profile_synced_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE users DROP COLUMN first_name,DROP COLUMN last_name,DROP COLUMN profile_revision,
 DROP COLUMN github_name,DROP COLUMN email,DROP COLUMN email_sync_error,DROP COLUMN github_profile_url,
 DROP COLUMN github_bio,DROP COLUMN github_company,DROP COLUMN github_location,
 DROP COLUMN github_created_at,DROP COLUMN profile_synced_at;
