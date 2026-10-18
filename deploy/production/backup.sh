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
# then is BACKUP_STATUS_FILE touched (the server exports its time as
# previa_backup_last_success_timestamp_seconds).
#
# With SENTRY_DSN set, every run checks in with the Sentry cron monitor
# "previa-backup" (created on the first run): in progress at the start, ok or
# error at the end. Sentry emails the operator when a run fails or none
# arrives within a day, which also covers a stopped timer or a dead host.
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
DSN=$(setting SENTRY_DSN)
PROJECT=${PREVIA_COMPOSE_PROJECT:-previa}

# The check-in URL comes from the DSN (https://KEY@HOST/PROJECT_ID). A
# check-in that cannot be sent never fails the backup itself.
CHECKIN_URL=
if [[ $DSN =~ ^(https?)://([^@/]+)@([^/]+)/([0-9]+)$ ]]; then
	CHECKIN_URL="${BASH_REMATCH[1]}://${BASH_REMATCH[3]}/api/${BASH_REMATCH[4]}/cron/previa-backup/${BASH_REMATCH[2]}/"
elif [[ -n $DSN ]]; then
	echo "SENTRY_DSN is not a Sentry DSN; no check-in is sent" >&2
fi
checkin() {
	[[ -n $CHECKIN_URL ]] || return 0
	curl -fsS -m 10 -o /dev/null "$CHECKIN_URL?status=$1" || echo "Sentry check-in ($1) not sent" >&2
}
# A daily interval, not a fixed time, so a changed timer schedule needs no
# change here; the margin covers the timer's randomized delay.
start_checkin() {
	[[ -n $CHECKIN_URL ]] || return 0
	curl -fsS -m 10 -o /dev/null -X POST -H 'Content-Type: application/json' --data \
		'{"status":"in_progress","monitor_config":{"schedule":{"type":"interval","value":1,"unit":"day"},"checkin_margin":60,"max_runtime":60,"failure_issue_threshold":1,"recovery_threshold":1}}' \
		"$CHECKIN_URL" || echo "Sentry check-in (in_progress) not sent" >&2
}

fail() {
	echo "backup failed: $1" >&2
	checkin error
	exit 1
}
trap 'fail "line $LINENO exited with status $?"' ERR
start_checkin

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
checkin ok
echo "backup $stamp complete"
