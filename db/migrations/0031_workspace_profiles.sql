-- +goose Up
ALTER TABLE workspaces
 ADD COLUMN description TEXT NOT NULL DEFAULT '' CHECK(char_length(description)<=240),
 ADD COLUMN profile_revision BIGINT NOT NULL DEFAULT 1 CHECK(profile_revision>0);

-- Logos are small, authenticated PNG assets. Keep their bytes out of workspace
-- selection and membership queries; profile changes do not revoke preview access.
CREATE TABLE workspace_logos (
 workspace_id BIGINT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
 version BIGINT NOT NULL CHECK(version>0),
 data BYTEA NOT NULL CHECK(octet_length(data)>0 AND octet_length(data)<=2097152)
);

-- +goose Down
DROP TABLE workspace_logos;
ALTER TABLE workspaces DROP COLUMN profile_revision, DROP COLUMN description;
