#!/usr/bin/env bash
# Upload a bounded, encrypted identifier-only journal before acknowledging
# completed deletions. This channel is independent of nightly database dumps.
set -Eeuo pipefail
umask 077
ENV_FILE=${PREVIA_ENV_FILE:-/etc/previa/previa.env}
COMPOSE_FILE=${PREVIA_COMPOSE_FILE:-$(dirname "$(readlink -f "$0")")/compose.yml}
PROJECT=${PREVIA_COMPOSE_PROJECT:-previa}
setting() { local value; value=$(grep -E "^$1=" "$ENV_FILE" | tail -n 1 | cut -d= -f2- || true); printf '%s' "${!1:-$value}"; }
RECIPIENT=$(setting BACKUP_AGE_RECIPIENT)
REMOTE=$(setting BACKUP_REMOTE)
[[ -n $RECIPIENT && -n $REMOTE ]] || { echo 'Deletion journal needs BACKUP_AGE_RECIPIENT and BACKUP_REMOTE' >&2; exit 1; }
command -v age >/dev/null
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# Prevent an older concurrent export from replacing a newer uploaded journal.
lock_dir=${PREVIA_JOURNAL_LOCK_DIR:-/var/lib/previa/backup}
mkdir -p "$lock_dir"
exec 9>"$lock_dir/deletion-journal.lock"
flock -w 60 9 || { echo 'Deletion journal channel is busy; retry later' >&2; exit 1; }
compose=(docker compose -p "$PROJECT" -f "$COMPOSE_FILE")
if [[ -n ${PREVIA_COMPOSE_OVERRIDE:-} ]]; then compose+=(-f "$PREVIA_COMPOSE_OVERRIDE"); fi
compose+=(--env-file "$ENV_FILE")
# run works during a restore/deploy even when the HTTP server is stopped.
"${compose[@]}" run --rm -T --no-deps journal journal-export >"$work/journal.json"
recipient_args=(-r "$RECIPIENT")
if [[ -f $RECIPIENT ]]; then recipient_args=(-R "$RECIPIENT"); fi
age "${recipient_args[@]}" -o "$work/latest.json.age" "$work/journal.json"
if [[ $REMOTE == /* ]]; then
 mkdir -p "$REMOTE/deletion-journal"
 cp "$work/latest.json.age" "$REMOTE/deletion-journal/latest.json.age.tmp"
 mv "$REMOTE/deletion-journal/latest.json.age.tmp" "$REMOTE/deletion-journal/latest.json.age"
else
 rclone copyto --quiet "$work/latest.json.age" "$REMOTE/deletion-journal/latest.json.age"
fi
# No acknowledgement on export, encryption or upload failure.
"${compose[@]}" run --rm -T --no-deps journal journal-ack <"$work/journal.json"
echo 'Deletion journal stored and acknowledged'
