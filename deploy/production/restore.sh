#!/usr/bin/env bash
# Restore Previa's database from a backup.sh dump, then bring the schema to
# this release (docs/24-production-deployment.md#backups-and-restore).
#
#   restore.sh --identity /path/to/age-key.txt --yes previa-20261005T020000Z.dump.age
#
# Destructive: the current database is replaced, so --yes is required. The
# server and worker are stopped first. Afterwards the worker starts with
# BUILDS_PAUSED=true: on start it rewrites the route file from the restored
# table and housekeeping reconciles the restored rows with the containers
# that actually exist, before any build is admitted. Unpause once /readyz and
# the Overview look right (the script prints the command).
set -Eeuo pipefail
umask 077

ENV_FILE=${PREVIA_ENV_FILE:-/etc/previa/previa.env}
COMPOSE_FILE=${PREVIA_COMPOSE_FILE:-$(dirname "$(readlink -f "$0")")/compose.yml}
PROJECT=${PREVIA_COMPOSE_PROJECT:-previa}

identity="" confirmed=false dump=""
while [[ $# -gt 0 ]]; do
	case $1 in
	--identity) identity=${2:?--identity needs a file}; shift 2 ;;
	--yes) confirmed=true; shift ;;
	-*) echo "unknown option $1" >&2; exit 2 ;;
	*) dump=$1; shift ;;
	esac
done
[[ -n $dump && -f $dump ]] || { echo "usage: restore.sh --identity KEY --yes DUMP.dump.age" >&2; exit 2; }
[[ -n $identity && -f $identity ]] || { echo "--identity must name the age private key file" >&2; exit 2; }
$confirmed || { echo "restore replaces the database; rerun with --yes" >&2; exit 2; }
command -v age >/dev/null || { echo "age is not installed" >&2; exit 1; }

compose=(docker compose -p "$PROJECT" -f "$COMPOSE_FILE")
if [[ -n ${PREVIA_COMPOSE_OVERRIDE:-} ]]; then
	compose+=(-f "$PREVIA_COMPOSE_OVERRIDE")
fi
compose+=(--env-file "$ENV_FILE")
started=$(date +%s)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

age -d -i "$identity" -o "$work/previa.dump" "$dump"
echo "decrypted $(basename "$dump") ($(stat -c %s "$work/previa.dump") bytes)"

"${compose[@]}" stop server worker
"${compose[@]}" up -d --wait postgres
psql() { "${compose[@]}" exec -T postgres psql -v ON_ERROR_STOP=1 -U previa "$@"; }
psql -d postgres -qc "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'previa' AND pid <> pg_backend_pid()" >/dev/null
psql -d postgres -qc "DROP DATABASE IF EXISTS previa"
psql -d postgres -qc "CREATE DATABASE previa OWNER previa"
"${compose[@]}" exec -T postgres pg_restore -U previa -d previa --no-owner --no-privileges --exit-on-error <"$work/previa.dump"
echo "restored the database"

# The backup may predate this release: apply newer migrations.
"${compose[@]}" run --rm migrate
BUILDS_PAUSED=true "${compose[@]}" up -d --wait worker
"${compose[@]}" up -d --wait server
echo "restore finished in $(($(date +%s) - started))s; builds are paused."
echo "Check the Overview and the worker's /readyz, then resume builds with:"
echo "  docker compose -p $PROJECT -f $COMPOSE_FILE --env-file $ENV_FILE up -d worker"
