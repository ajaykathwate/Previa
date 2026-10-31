-- name: UpsertUser :one
INSERT INTO users(github_id,username,avatar_url,first_name,last_name,github_name,email,email_sync_error,github_profile_url,github_bio,github_company,github_location,github_created_at,profile_synced_at)
VALUES(sqlc.arg(github_id),sqlc.arg(username),sqlc.arg(avatar_url),sqlc.arg(first_name),sqlc.arg(last_name),sqlc.arg(github_name),sqlc.arg(email),sqlc.arg(email_sync_error),sqlc.arg(github_profile_url),sqlc.arg(github_bio),sqlc.arg(github_company),sqlc.arg(github_location),sqlc.narg(github_created_at),now())
ON CONFLICT(github_id) DO UPDATE SET username=EXCLUDED.username,avatar_url=EXCLUDED.avatar_url,github_name=EXCLUDED.github_name,
-- Prefill a never-edited, still-blank name from GitHub; a saved choice is kept.
first_name=CASE WHEN users.profile_revision=1 AND users.first_name='' AND users.last_name='' THEN EXCLUDED.first_name ELSE users.first_name END,
last_name=CASE WHEN users.profile_revision=1 AND users.first_name='' AND users.last_name='' THEN EXCLUDED.last_name ELSE users.last_name END,
email=CASE WHEN EXCLUDED.email_sync_error THEN users.email ELSE EXCLUDED.email END,email_sync_error=EXCLUDED.email_sync_error,
github_profile_url=EXCLUDED.github_profile_url,github_bio=EXCLUDED.github_bio,github_company=EXCLUDED.github_company,github_location=EXCLUDED.github_location,
github_created_at=EXCLUDED.github_created_at,profile_synced_at=EXCLUDED.profile_synced_at
WHERE users.deleted_at IS NULL
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id=$1 AND deleted_at IS NULL;

-- name: AdvanceSessionEpoch :one
UPDATE users SET session_epoch=session_epoch+1 WHERE id=$1 AND deleted_at IS NULL RETURNING session_epoch;

-- name: UpdatePersonalName :execrows
UPDATE users SET first_name=$2,last_name=$3,profile_revision=profile_revision+1 WHERE id=$1 AND profile_revision=$4;

-- name: SaveAvatar :one
WITH saved AS (
 INSERT INTO user_avatars(user_id,data) VALUES(sqlc.arg(user_id),sqlc.arg(data))
 ON CONFLICT(user_id) DO UPDATE SET data=EXCLUDED.data RETURNING user_id
)
UPDATE users u SET avatar_version=u.avatar_version+1,custom_avatar=true FROM saved WHERE u.id=saved.user_id RETURNING u.avatar_version;

-- name: RemoveAvatar :exec
WITH removed AS (DELETE FROM user_avatars WHERE user_id=$1)
UPDATE users SET custom_avatar=false WHERE id=$1;

-- name: ReadAvatar :one
SELECT a.data,u.avatar_version FROM user_avatars a JOIN users u ON u.id=a.user_id WHERE a.user_id=$1 AND u.custom_avatar;

-- name: GetSession :one
-- Session CRUD is owned by SCS pgxstore. This query verifies persisted sessions.
SELECT token,data,expiry FROM sessions WHERE token=$1;

-- name: UpsertGoogleUser :one
INSERT INTO users(google_subject,google_email,username,first_name,email,profile_synced_at)
VALUES($1,$2,$3,$4,$2,clock_timestamp())
ON CONFLICT(google_subject) DO UPDATE SET google_email=EXCLUDED.google_email,
 email=CASE WHEN users.github_id IS NULL THEN EXCLUDED.email ELSE users.email END,
 profile_synced_at=CASE WHEN users.github_id IS NULL THEN clock_timestamp() ELSE users.profile_synced_at END WHERE users.deleted_at IS NULL RETURNING *;

-- name: LinkGoogleUser :one
UPDATE users SET google_subject=$2,google_email=$3 WHERE id=$1 AND deleted_at IS NULL
AND (google_subject IS NULL OR google_subject=$2) RETURNING *;

-- name: LinkGitHubUser :one
UPDATE users SET github_id=$2,username=$3,avatar_url=$4,github_name=$5,email=$6,email_sync_error=$7,
 github_profile_url=$8,profile_synced_at=clock_timestamp()
WHERE id=$1 AND deleted_at IS NULL AND (github_id IS NULL OR github_id=$2) RETURNING *;
