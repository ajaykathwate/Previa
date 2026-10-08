#!/usr/bin/env bash
# Nightly Previa backup (docs/25-production-deployment.md#backups-and-restore).
#
# 1. pg_dump of the whole database (custom format: one consistent snapshot).
# 2. The router's ACME state (account key and certificates).
# 3. /etc/previa: settings, the repository-secrets keyring and the App key.
#    Without the keyring, restored repository secrets cannot be decrypted.
#
# Every file is encrypted with age to BACKUP_AGE_RECIPIENT before it leaves
# the host; the matching private key is kept offline. Files go to
# BACKUP_REMOTE (an rclone remote such as s3:previa-backups, or an absolute
# directory), copies older than BACKUP_RETENTION_DAYS are deleted, and only
# then is BACKUP_STATUS_FILE touched; the server turns its time into
# previa_backup_last_success_timestamp_seconds and Prometheus alerts when it
# is a day old. A failure posts to BACKUP_FAILURE_WEBHOOK when set.
# Nothing is printed except file names and sizes.
set -Eeuo pipefail
umask 077

ENV_FILE=${PREVIA_ENV_FILE:-/etc/previa/previa.env}
COMPOSE_FILE=${PREVIA_COMPOSE_FILE:-$(dirname "$(readlink -f "$0")")/compose.yml}
CONFIG_DIR=${PREVIA_CONFIG_DIR:-/etc/previa}
STATUS_DIR=${PREVIA_BACKUP_STATUS_DIR:-/var/lib/previa/backup}

# Read only the backup settings from the env file (it is not sourced: its
# other values are secrets this script has no business holding).
setting() {
	local value
	value=$(grep -E "^$1=" "$ENV_FILE" | tail -n 1 | cut -d= -f2- || true)
	printf '%s' "${!1:-$value}"
}
RECIPIENT=$(setting BACKUP_AGE_RECIPIENT)
REMOTE=$(setting BACKUP_REMOTE)
RETENTION=$(setting BACKUP_RETENTION_DAYS)
RETENTION=${RETENTION:-30}
WEBHOOK=$(setting BACKUP_FAILURE_WEBHOOK)
PROJECT=${PREVIA_COMPOSE_PROJECT:-previa}

fail() {
	echo "backup failed: $1" >&2
	if [[ -n $WEBHOOK ]]; then
		curl -fsS -m 10 -X POST -H 'Content-Type: application/json' \
			-d "{\"text\":\"Previa backup failed on $(hostname): $1\"}" "$WEBHOOK" >/dev/null || true
	fi
	exit 1
}
trap 'fail "line $LINENO exited with status $?"' ERR

[[ -n $RECIPIENT ]] || fail "BACKUP_AGE_RECIPIENT is not set"
[[ -n $REMOTE ]] || fail "BACKUP_REMOTE is not set"
[[ $RETENTION =~ ^[0-9]+$ && $RETENTION -ge 1 ]] || fail "BACKUP_RETENTION_DAYS must be a positive number of days"
command -v age >/dev/null || fail "age is not installed"
if [[ $REMOTE != /* ]]; then
	command -v rclone >/dev/null || fail "rclone is not installed (or use an absolute directory as BACKUP_REMOTE)"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
stamp=$(date -u +%Y%m%dT%H%M%SZ)
recipient_args=(-r "$RECIPIENT")
if [[ -f $RECIPIENT ]]; then
	recipient_args=(-R "$RECIPIENT")
fi

compose=(docker compose -p "$PROJECT" -f "$COMPOSE_FILE")
if [[ -n ${PREVIA_COMPOSE_OVERRIDE:-} ]]; then
	compose+=(-f "$PREVIA_COMPOSE_OVERRIDE")
fi
compose+=(--env-file "$ENV_FILE")
"${compose[@]}" exec -T postgres pg_dump -U previa -d previa --format=custom --compress=6 --no-owner --no-privileges |
	age "${recipient_args[@]}" -o "$work/previa-$stamp.dump.age"
# A dump of a migrated database is never tiny: catch an empty pipe early.
[[ $(stat -c %s "$work/previa-$stamp.dump.age") -gt 1024 ]] || fail "the database dump is suspiciously small"

docker run --rm --network none -v "${PROJECT}_acme:/acme:ro" --entrypoint tar \
	"$(docker inspect --format '{{.Config.Image}}' "${PREVIA_ROUTER_CONTAINER:-previa-router}")" -C /acme -czf - . |
	age "${recipient_args[@]}" -o "$work/previa-acme-$stamp.tar.gz.age"
tar -C "$CONFIG_DIR" -czf - . | age "${recipient_args[@]}" -o "$work/previa-config-$stamp.tar.gz.age"

for file in "$work"/*.age; do
	if [[ $REMOTE == /* ]]; then
		mkdir -p "$REMOTE"
		cp "$file" "$REMOTE/"
	else
		rclone copyto --quiet "$file" "$REMOTE/$(basename "$file")"
	fi
	echo "stored $(basename "$file") ($(stat -c %s "$file") bytes)"
done

if [[ $REMOTE == /* ]]; then
	find "$REMOTE" -maxdepth 1 -name 'previa-*.age' -mtime "+$RETENTION" -delete
else
	rclone delete --quiet --min-age "${RETENTION}d" --include 'previa-*.age' "$REMOTE"
fi

mkdir -p "$STATUS_DIR"
chmod 755 "$STATUS_DIR"
date -u +%Y-%m-%dT%H:%M:%SZ >"$STATUS_DIR/last-success.tmp"
chmod 644 "$STATUS_DIR/last-success.tmp"
mv "$STATUS_DIR/last-success.tmp" "$STATUS_DIR/last-success"
echo "backup $stamp complete"
