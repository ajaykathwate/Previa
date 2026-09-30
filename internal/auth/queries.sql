-- name: UpsertUser :one
INSERT INTO users (github_id, username, avatar_url)
VALUES ($1, $2, $3)
ON CONFLICT (github_id) DO UPDATE
SET username = EXCLUDED.username, avatar_url = EXCLUDED.avatar_url
RETURNING id, github_id, username, avatar_url;

-- name: GetUser :one
SELECT id, github_id, username, avatar_url FROM users WHERE id = $1;

-- name: GetSession :one
-- Session CRUD is owned by SCS pgxstore. This query verifies persisted sessions.
SELECT token, data, expiry FROM sessions WHERE token = $1;
