-- +goose Up
-- The head commit's message and author, shown on the PR page. GitHub's pull
-- request payloads omit them, so the page fetches them once per head SHA.
ALTER TABLE pull_requests
 ADD COLUMN head_commit_sha TEXT NOT NULL DEFAULT '',
 ADD COLUMN head_commit_message TEXT NOT NULL DEFAULT '' CHECK(char_length(head_commit_message)<=4000),
 ADD COLUMN head_commit_author TEXT NOT NULL DEFAULT '',
 ADD COLUMN head_commit_at TIMESTAMPTZ,
 ADD COLUMN head_commit_checked_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE pull_requests DROP COLUMN head_commit_checked_at, DROP COLUMN head_commit_at,
 DROP COLUMN head_commit_author, DROP COLUMN head_commit_message, DROP COLUMN head_commit_sha;
