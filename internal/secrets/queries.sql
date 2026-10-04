-- name: OwnedRepository :one
SELECT r.id, r.full_name, (r.accessible AND i.status = 'active')::boolean AS available
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = $1 AND i.owner_user_id = $2;

-- name: LockOwnedRepository :one
SELECT r.id, (r.accessible AND i.status = 'active')::boolean AS available
FROM repos r JOIN installations i ON i.id = r.installation_id
WHERE r.id = $1 AND i.owner_user_id = $2
FOR UPDATE OF r;

-- name: ListSecrets :many
SELECT key, updated_at FROM env_secrets WHERE repo_id = $1 ORDER BY key;

-- name: CountSecrets :one
SELECT count(*) FROM env_secrets WHERE repo_id = $1;

-- name: SecretExists :one
SELECT EXISTS (SELECT 1 FROM env_secrets WHERE repo_id = $1 AND key = $2);

-- name: UpsertSecret :exec
INSERT INTO env_secrets (repo_id, key, encrypted_value, key_version)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repo_id, key) DO UPDATE
SET encrypted_value = EXCLUDED.encrypted_value, key_version = EXCLUDED.key_version, updated_at = now();

-- name: DeleteOwnedSecret :execrows
DELETE FROM env_secrets s
USING repos r JOIN installations i ON i.id = r.installation_id
WHERE s.repo_id = r.id AND r.id = $1 AND i.owner_user_id = $2 AND s.key = $3;

-- name: SealedSecrets :many
SELECT key, encrypted_value, key_version FROM env_secrets WHERE repo_id = $1 ORDER BY key;

-- name: SecretsNeedingRotation :many
SELECT id, repo_id, key, encrypted_value, key_version FROM env_secrets
WHERE key_version <> sqlc.arg(active_version) AND id > sqlc.arg(after_id)
ORDER BY id LIMIT sqlc.arg(batch_size);

-- name: RotateSecret :execrows
UPDATE env_secrets SET encrypted_value = sqlc.arg(new_value), key_version = sqlc.arg(new_version)
WHERE id = sqlc.arg(id) AND key_version = sqlc.arg(old_version) AND encrypted_value = sqlc.arg(old_value);

-- name: CountSecretsByKeyVersion :many
SELECT key_version, count(*) AS secrets FROM env_secrets GROUP BY key_version ORDER BY key_version;
