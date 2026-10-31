-- +goose Up
-- A member may replace their GitHub picture with an uploaded photo. The version
-- only grows, so each upload gets a new cacheable address; custom_avatar says
-- whether the uploaded photo or the GitHub picture is shown.
ALTER TABLE users
 ADD COLUMN avatar_version BIGINT NOT NULL DEFAULT 0 CHECK(avatar_version>=0),
 ADD COLUMN custom_avatar BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE user_avatars (
 user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 data BYTEA NOT NULL CHECK(octet_length(data)>0 AND octet_length(data)<=524288)
);

-- Members who never edited their name start from GitHub's: first word, rest.
WITH split AS (
 SELECT id, regexp_replace(btrim(github_name),'\s+',' ','g') AS name FROM users
 WHERE profile_revision=1 AND first_name='' AND last_name='' AND btrim(github_name)<>''
), parts AS (
 SELECT id, split_part(name,' ',1) AS first, btrim(substr(name, length(split_part(name,' ',1))+1)) AS last FROM split
)
UPDATE users u SET first_name=parts.first, last_name=parts.last FROM parts
WHERE u.id=parts.id AND char_length(parts.first)<=80 AND char_length(parts.last)<=80;

-- +goose Down
DROP TABLE user_avatars;
ALTER TABLE users DROP COLUMN custom_avatar, DROP COLUMN avatar_version;
