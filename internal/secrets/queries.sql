-- name: OwnedRepository :one
SELECT r.id, r.full_name, (r.accessible AND i.status = 'active')::boolean AS available
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = $1 AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'manage');

-- name: LockOwnedRepository :one
SELECT r.id, (r.accessible AND i.status = 'active')::boolean AS available
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = $1 AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'manage')
FOR UPDATE OF r;

-- name: ListSecrets :many
SELECT key, updated_at, build FROM env_secrets WHERE repo_id = $1 ORDER BY key;

-- name: CountSecrets :one
SELECT count(*) FROM env_secrets WHERE repo_id = $1;

-- name: ExistingSecretNames :many
SELECT key FROM env_secrets WHERE repo_id = sqlc.arg(repo_id) AND key = ANY(sqlc.arg(keys)::text[]);

-- name: UpsertSecret :exec
INSERT INTO env_secrets (repo_id, key, encrypted_value, key_version)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repo_id, key) DO UPDATE
SET encrypted_value = EXCLUDED.encrypted_value, key_version = EXCLUDED.key_version, updated_at = now();

-- name: DeleteOwnedSecrets :execrows
DELETE FROM env_secrets s
USING repos r JOIN installations i ON i.id = r.installation_id
WHERE s.repo_id = r.id AND r.id = sqlc.arg(id) AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'manage')
  AND s.key = ANY(sqlc.arg(keys)::text[]);

-- name: SealedSecrets :many
SELECT key, encrypted_value, key_version, build FROM env_secrets WHERE repo_id = $1 ORDER BY key;

-- name: SetSecretBuild :execrows
-- Whether Dockerfile steps of trusted builds may read the secret.
UPDATE env_secrets s SET build = sqlc.arg(build)::boolean, updated_at = now()
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE s.repo_id = r.id AND r.id = sqlc.arg(repo_id) AND workspace_repository_access(r.id,sqlc.narg(owner_user_id)::bigint,'manage')
  AND s.key = sqlc.arg(key) AND s.build IS DISTINCT FROM sqlc.arg(build)::boolean
  AND (NOT sqlc.arg(build)::boolean OR (r.accessible AND i.status = 'active'));

-- name: SecretsNeedingRotation :many
SELECT id, repo_id, key, encrypted_value, key_version FROM env_secrets
WHERE key_version <> sqlc.arg(active_version) AND id > sqlc.arg(after_id)
ORDER BY id LIMIT sqlc.arg(batch_size);

-- name: RotateSecret :execrows
UPDATE env_secrets SET encrypted_value = sqlc.arg(new_value), key_version = sqlc.arg(new_version)
WHERE id = sqlc.arg(id) AND key_version = sqlc.arg(old_version) AND encrypted_value = sqlc.arg(old_value);

-- name: CountSecretsByKeyVersion :many
SELECT key_version, count(*) AS secrets FROM env_secrets GROUP BY key_version ORDER BY key_version;

-- name: LockSecretInstallation :one
SELECT i.id FROM installations i JOIN repos r ON r.installation_id=i.id WHERE r.id=$1 FOR UPDATE OF i;

-- name: EnvFile :one
SELECT f.encrypted_text, f.key_version, f.revision, f.updated_at, coalesce(u.username, '')::text AS updated_by
FROM repository_env_files f LEFT JOIN users u ON u.id = f.updated_by
WHERE f.repo_id = $1;

-- name: CreateEnvFile :one
-- The first save; no row means someone else saved a file first.
INSERT INTO repository_env_files (repo_id, encrypted_text, key_version, updated_by)
VALUES (sqlc.arg(repo_id), sqlc.arg(encrypted_text), sqlc.arg(key_version), sqlc.narg(updated_by))
ON CONFLICT (repo_id) DO NOTHING
RETURNING revision;

-- name: UpdateEnvFile :one
-- Saves only while the file is still at the revision its editor opened; no
-- row means someone else saved meanwhile.
UPDATE repository_env_files
SET encrypted_text = sqlc.arg(encrypted_text), key_version = sqlc.arg(key_version),
    revision = revision + 1, updated_by = sqlc.narg(updated_by), updated_at = clock_timestamp()
WHERE repo_id = sqlc.arg(repo_id) AND revision = sqlc.arg(expected_revision)::bigint
RETURNING revision;

-- name: DeleteSecretsNotIn :exec
DELETE FROM env_secrets WHERE repo_id = sqlc.arg(repo_id) AND NOT (key = ANY(sqlc.arg(keys)::text[]));

-- name: EnvFilesNeedingRotation :many
SELECT repo_id, encrypted_text, key_version FROM repository_env_files
WHERE key_version <> sqlc.arg(active_version) AND repo_id > sqlc.arg(after_repo_id)
ORDER BY repo_id LIMIT sqlc.arg(batch_size);

-- name: RotateEnvFile :execrows
UPDATE repository_env_files SET encrypted_text = sqlc.arg(new_text), key_version = sqlc.arg(new_version)
WHERE repo_id = sqlc.arg(repo_id) AND key_version = sqlc.arg(old_version) AND encrypted_text = sqlc.arg(old_text);

-- name: CountEnvFilesByKeyVersion :many
SELECT key_version, count(*) AS files FROM repository_env_files GROUP BY key_version ORDER BY key_version;
