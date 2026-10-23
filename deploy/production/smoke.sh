#!/usr/bin/env bash
# Several checks run in a child bash whose expressions must expand there.
# shellcheck disable=SC2016
# Local staging smoke test of the production stack (make prod-smoke):
# the release images, production Compose (with compose.smoke.yml), real
# PostgreSQL, migrations, the router serving the dashboard over HTTPS behind
# a trusted proxy, readiness, metrics, Prometheus and Grafana, container
# hardening, restarts, a graceful stop, and a backup and restore drill with
# measured duration and data-loss window. Everything runs under the project
# "previa-smoke" and is removed at the end.
#
# Needs the images (make release-images PREVIA_VERSION=...), and age for the
# drill. It writes nothing outside a temporary directory.
set -Eeuo pipefail

VERSION=${PREVIA_VERSION:?set PREVIA_VERSION to the built images tag}
HERE=$(dirname "$(readlink -f "$0")")
# The toolchain the release images are built with, from the Dockerfile's pin.
GO_VERSION=$(sed -n 's/^ARG GO_IMAGE=golang:\([0-9.]*\)-.*/go\1/p' "$HERE/../../Dockerfile")
[[ -n $GO_VERSION ]] || { echo "no Go version in the Dockerfile's GO_IMAGE" >&2; exit 1; }
SCHEMA_VERSION=$(sed -n 's/^const SchemaVersion = \([0-9]*\)$/\1/p' "$HERE/../../internal/platform/database/schema.go")
[[ $SCHEMA_VERSION =~ ^[0-9]+$ ]] || { echo "no schema version in the application" >&2; exit 1; }
ALERT_RULE_COUNT=$(grep -Ec '^[[:space:]]+- alert:' "$HERE/../observability/alerts.yml")
[[ $ALERT_RULE_COUNT -gt 0 ]] || { echo "no alert rules configured" >&2; exit 1; }
PROJECT=previa-smoke
CURL_IMAGE=curlimages/curl:8.15.0@sha256:4026b29997dc7c823b51c164b71e2b51e0fd95cce4601f78202c513d97da2922
HOST=previa.localtest.me
work=$(mktemp -d)
chmod 755 "$work"
compose=(docker compose -p "$PROJECT" -f "$HERE/compose.yml" -f "$HERE/compose.smoke.yml" --env-file "$work/previa.env" --profile observability)
passed=0

cleanup() {
	if [[ ${SMOKE_KEEP:-} == 1 ]]; then
		echo "left running for inspection (SMOKE_KEEP=1): docker compose -p $PROJECT ... down -v; rm -rf $work" >&2
		return
	fi
	"${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
check() { # check NAME COMMAND...: run, count, and stop at the first failure
	local name=$1
	shift
	if "$@"; then
		passed=$((passed + 1))
		echo "ok   $name"
	else
		echo "FAIL $name" >&2
		"${compose[@]}" ps >&2 || true
		"${compose[@]}" logs --tail 40 server worker router >&2 || true
		exit 1
	fi
}
inside() { docker run --rm --network "${PROJECT}_control" "$CURL_IMAGE" -fsS --max-time 10 "$@"; }
outside() { curl -ksS --max-time 10 --resolve "$HOST:18443:127.0.0.1" --resolve "pr-1-x-y.preview.localtest.me:18443:127.0.0.1" "$@"; }
psql() { "${compose[@]}" exec -T postgres psql -v ON_ERROR_STOP=1 -U previa -d previa -tAc "$1"; }
internal_ports_unpublished() {
	local service id bindings
	for service in server worker; do
		id=$("${compose[@]}" ps -q "$service") || return 1
		bindings=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$id") || return 1
		[[ $bindings == null || $bindings == '{}' ]] || return 1
	done
}
# A failed restore must leave the public router and every writer stopped.
restore_refuses_missing_journal() {
	mv "$work/remote/deletion-journal/latest.json.age" "$work/journal.saved.age" || return 1
	local rejected=false service
	if ! env "${drill_env[@]}" "$HERE/restore.sh" --identity "$work/age-key.txt" --yes "$dump" >"$work/missing-journal.log" 2>&1; then
		rejected=true
	fi
	mv "$work/journal.saved.age" "$work/remote/deletion-journal/latest.json.age" || return 1
	$rejected || return 1
	for service in server worker router; do
		[[ -z $("${compose[@]}" ps --status running -q "$service") ]] || return 1
	done
}
# Checks that run in a child bash use these helpers too.
export HOST PROJECT CURL_IMAGE VERSION GO_VERSION
export -f inside outside

echo "== configuration in $work"
mkdir -p "$work/config" "$work/router" "$work/backup" "$work/remote"
chmod 777 "$work/router" # the worker (UID 65532) writes the route file here
chmod 755 "$work/backup"
printf 'not-a-real-key\n' >"$work/config/github-app.pem"
printf 'not-a-real-token\n' >"$work/config/dns-credential"
chmod 600 "$work/config/"*
postgres_password=$(openssl rand -hex 16)
cat >"$work/previa.env" <<ENV
PREVIA_VERSION=$VERSION
PREVIA_ENV_FILE=$work/previa.env
PREVIA_ROUTER_DIR=$work/router
PREVIA_BACKUP_STATUS_DIR=$work/backup
DOCKER_GID=$(stat -c %g /var/run/docker.sock)
POSTGRES_PASSWORD=$postgres_password
DATABASE_URL=postgres://previa:$postgres_password@postgres:5432/previa?sslmode=disable
APP_BASE_URL=https://$HOST:18443
PREVIA_DASHBOARD_HOST=$HOST
SESSION_SECRET=$(openssl rand -hex 32)
GITHUB_OAUTH_CLIENT_ID=smoke-client
GITHUB_OAUTH_CLIENT_SECRET=smoke-secret
GITHUB_WEBHOOK_SECRET=$(openssl rand -hex 32)
SUPPORT_OPERATOR_GITHUB_IDS=1
GITHUB_APP_PRIVATE_KEY_FILE=$work/config/github-app.pem
SECRETS_ENCRYPTION_KEYS=1:$(openssl rand -base64 32)
SECRETS_ACTIVE_KEY_VERSION=1
PREVIEW_BASE_DOMAIN=preview.localtest.me
MAX_CONCURRENT_BUILDS=1
ACME_EMAIL=ops@example.com
ACME_DNS_PROVIDER=cloudflare
ACME_DNS_CREDENTIAL_VARIABLE=CF_DNS_API_TOKEN
ACME_DNS_CREDENTIAL_FILE=$work/config/dns-credential
ACME_CA_SERVER=https://127.0.0.1:1/directory
GRAFANA_ADMIN_PASSWORD=$(openssl rand -hex 12)
LOG_LEVEL=info
ENV
chmod 600 "$work/previa.env"

echo "== start (images $VERSION)"
started=$(date +%s)
check "stack starts and every health check passes" "${compose[@]}" up -d --wait --wait-timeout 180
echo "     started in $(($(date +%s) - started))s"
check "migrations ran once and exited cleanly" test "$(docker inspect --format '{{.State.ExitCode}}' "${PROJECT}-migrate-1")" = 0
check "schema is at the release's migration" test "$(psql "SELECT max(version_id) FROM goose_db_version WHERE is_applied")" = "$SCHEMA_VERSION"
check "the route file is written from the accepted table" \
	"${compose[@]}" run --rm --no-deps --entrypoint /usr/local/bin/previa-route-sync worker

echo "== through the router (HTTPS, trusted proxy, host validation)"
check "dashboard home over HTTPS" bash -c "outside -o /dev/null -w '%{http_code}' https://$HOST:18443/ | grep -qx 200"
headers=$(outside -D - -o /dev/null "https://$HOST:18443/")
check "Content-Security-Policy with a nonce" grep -qi "^content-security-policy: .*'nonce-" <<<"$headers"
check "HSTS on the HTTPS dashboard" grep -qi '^strict-transport-security: max-age=' <<<"$headers"
check "framing refused" grep -qi '^x-frame-options: DENY' <<<"$headers"
check "a spoofed X-Forwarded-Host is replaced by the router" \
	bash -c "outside -H 'X-Forwarded-Host: evil.example' -o /dev/null -w '%{http_code}' https://$HOST:18443/ | grep -qx 200"
# Traefik loads the route file route-sync just wrote a moment later (file
# watch plus its reload throttle), so allow it a few seconds.
check "an unknown preview host goes to the unavailable page" bash -c '
	for _ in $(seq 20); do
		outside -o /dev/null -w "%{redirect_url}" https://pr-1-x-y.preview.localtest.me:18443/ | grep -q /previews/unavailable && exit 0
		sleep 0.5
	done
	exit 1'
check "sign-in is rate limited per client" bash -c '
	for _ in $(seq 25); do outside -o /dev/null -w "%{http_code}\n" "https://'"$HOST"':18443/auth/github/login"; done | grep -qx 429'
check "internal listeners are not published" internal_ports_unpublished

echo "== inside the control network"
ready=$(inside http://server:9090/readyz)
check "server ready, schema current, version $VERSION" grep -q "\"status\":\"ready\".*\"version\":\"$VERSION\"" <<<"$(tr -d '\n' <<<"$ready")"
ready=$(inside http://worker:9091/readyz)
check "worker ready and reports its capabilities" grep -q '"capabilities":{"execution":false' <<<"$ready"
# State gauges come from a snapshot refreshed at most every 10 seconds, so
# one taken just before the worker registered still says none is live.
check "build info and state gauges exported" bash -c '
	for _ in $(seq 15); do
		metrics=$(inside http://server:9090/metrics) &&
			grep -q "previa_build_info{go_version=\"$GO_VERSION\",version=\"$VERSION\"} 1" <<<"$metrics" &&
			grep -q "^previa_workers_live 1" <<<"$metrics" && exit 0
		sleep 1
	done
	exit 1'
check "Prometheus scrapes server, worker and router" bash -c '
	for _ in $(seq 20); do
		up=$(docker run --rm --network '"${PROJECT}_control"' '"$CURL_IMAGE"' -fsS "http://prometheus:9090/api/v1/query?query=sum(up)")
		grep -q "\"value\":\[[0-9.]*,\"3\"\]" <<<"$up" && exit 0
		sleep 3
	done
	exit 1'
check "all $ALERT_RULE_COUNT alert rules loaded" bash -c "inside http://prometheus:9090/api/v1/rules | grep -o '\"type\":\"alerting\"' | wc -l | grep -qx $ALERT_RULE_COUNT"
grafana_password=$(grep '^GRAFANA_ADMIN_PASSWORD=' "$work/previa.env" | cut -d= -f2-)
# Grafana provisions its dashboards in the background as it starts.
check "Grafana provisioned the Previa dashboard" bash -c '
	for _ in $(seq 10); do
		curl -fsS --max-time 10 -u "admin:$1" "http://127.0.0.1:13000/api/search?query=Previa" | grep -q "\"uid\":\"previa\"" && exit 0
		sleep 2
	done
	exit 1' _ "$grafana_password"

echo "== container hardening"
for service in server worker; do
	id=$("${compose[@]}" ps -q "$service")
	check "$service runs as UID 65532 on a read-only root without capabilities" \
		test "$(docker inspect --format '{{.Config.User}} {{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}}' "$id")" = "65532:65532 true [ALL]"
done
check "the server has no Docker socket" bash -c "! docker inspect --format '{{range .Mounts}}{{.Source}} {{end}}' $("${compose[@]}" ps -q server) | grep -q docker.sock"
check "the dashboard database port is not published" bash -c "test -z \"\$(docker port $("${compose[@]}" ps -q postgres))\""

echo "== restarts and graceful stop"
check "the server comes back healthy after a restart" "${compose[@]}" restart server
check "the server is healthy again" "${compose[@]}" up -d --wait --wait-timeout 60 server
worker=$("${compose[@]}" ps -q worker)
stop_started=$(date +%s)
check "the worker stops gracefully" "${compose[@]}" stop worker
check "the worker drained and exited 0" test "$(docker inspect --format '{{.State.ExitCode}}' "$worker")" = 0
check "it said so" bash -c "docker logs $worker 2>&1 | grep -q 'River worker stopped'"
echo "     stopped in $(($(date +%s) - stop_started))s"
check "the worker starts again" "${compose[@]}" up -d --wait --wait-timeout 90 worker

echo "== upgrade and rollback rehearsal"
# The same procedure as a release upgrade (docs/25-production-deployment.md):
# a new tag is rolled out (migrations run first), then rolled back.
for image in server worker egress; do docker tag "previa-$image:$VERSION" "previa-$image:$VERSION-next"; done
image_of() { docker inspect --format '{{.Config.Image}}' "$("${compose[@]}" ps -q "$1")"; }
check "upgrade: the stack rolls to the new tag" env PREVIA_VERSION="$VERSION-next" "${compose[@]}" up -d --wait --wait-timeout 180
check "upgrade: server and worker run the new images" test "$(image_of server) $(image_of worker)" = "previa-server:$VERSION-next previa-worker:$VERSION-next"
check "upgrade: migrations ran again and changed nothing" test "$(psql "SELECT max(version_id) FROM goose_db_version WHERE is_applied")" = "$SCHEMA_VERSION"
check "rollback: the stack returns to the previous tag" "${compose[@]}" up -d --wait --wait-timeout 180
check "rollback: server and worker run the previous images" test "$(image_of server) $(image_of worker)" = "previa-server:$VERSION previa-worker:$VERSION"
for image in server worker egress; do docker rmi -f "previa-$image:$VERSION-next" >/dev/null; done

if command -v age >/dev/null && command -v age-keygen >/dev/null; then
	echo "== backup and restore drill"
	age-keygen -o "$work/age-key.txt" 2>/dev/null
	recipient=$(age-keygen -y "$work/age-key.txt")
	drill_env=(PREVIA_ENV_FILE="$work/previa.env" PREVIA_COMPOSE_PROJECT="$PROJECT" PREVIA_COMPOSE_OVERRIDE="$HERE/compose.smoke.yml"
		PREVIA_CONFIG_DIR="$work/config" PREVIA_BACKUP_STATUS_DIR="$work/backup" PREVIA_ROUTER_CONTAINER=previa-smoke-router
		PREVIA_JOURNAL_LOCK_DIR="$work/backup" BACKUP_AGE_RECIPIENT="$recipient" BACKUP_REMOTE="$work/remote")
	psql "INSERT INTO users (github_id, username) VALUES (900001, 'before-backup')" >/dev/null
	psql "INSERT INTO users (github_id, username,first_name) VALUES (900003, 'erase-after-backup','Private')" >/dev/null
	psql "INSERT INTO sessions(token,data,expiry) VALUES('restore-session-proof',decode('00','hex'),clock_timestamp()+interval '1 day')" >/dev/null
	backup_started=$(date +%s)
	check "backup.sh stores encrypted dumps of the database, ACME state and settings" env "${drill_env[@]}" "$HERE/backup.sh"
	echo "     backup took $(($(date +%s) - backup_started))s"
	check "nothing in the backup is readable without the key" bash -c "! grep -aq before-backup $work/remote/*.age"
	check "the server reports the backup's time" bash -c '
		for _ in $(seq 10); do
			docker run --rm --network '"${PROJECT}_control"' '"$CURL_IMAGE"' -fsS http://server:9090/metrics | grep -q "^previa_backup_last_success_timestamp_seconds [0-9]" && exit 0
			sleep 3
		done
		exit 1'
	psql "INSERT INTO users (github_id, username) VALUES (900002, 'after-backup')" >/dev/null
	# The deletion occurs AFTER the dump. Only the independently uploaded journal
	# can prevent this identity and its profile from reappearing on restore.
	psql "WITH u AS (
	 UPDATE users SET deleted_at=clock_timestamp(),session_epoch=session_epoch+1 WHERE github_id=900003 RETURNING id,public_id,github_id
	), o AS (
	 INSERT INTO lifecycle_operations(kind,target_id,target_public_id,actor_id) SELECT 'account',id,public_id,id FROM u RETURNING id,target_public_id
	) INSERT INTO deletion_journal(action,target,github_user_id,operation_id) SELECT 'account',u.public_id,u.github_id,o.id FROM u JOIN o ON o.target_public_id=u.public_id" >/dev/null
	check "the independent encrypted journal acknowledges the post-backup deletion" env "${drill_env[@]}" "$HERE/deletion-journal.sh"
	check "no deletion completes without its uploaded journal" test "$(psql "SELECT count(*) FROM lifecycle_operations WHERE kind='account' AND journal_ack_at IS NOT NULL")" = 1
	dump=$(ls "$work/remote"/previa-2*.dump.age)
	check "a missing independent journal keeps restore and traffic stopped" restore_refuses_missing_journal
	restore_started=$(date +%s)
	check "restore.sh restores, migrates and restarts with builds paused" \
		env "${drill_env[@]}" "$HERE/restore.sh" --identity "$work/age-key.txt" --yes "$dump"
	restore_seconds=$(($(date +%s) - restore_started))
	check "rows from before the backup are back" test "$(psql "SELECT count(*) FROM users WHERE username = 'before-backup'")" = 1
	check "rows written after the backup are gone (the data-loss window)" test "$(psql "SELECT count(*) FROM users WHERE username = 'after-backup'")" = 0
	check "post-backup account revocation survives restore before serving" test "$(psql "SELECT count(*) FROM users WHERE github_id=900003 AND deleted_at IS NOT NULL AND session_epoch>0")" = 1
	check "restore never resurrects previously issued dashboard sessions" test "$(psql "SELECT count(*) FROM sessions WHERE token='restore-session-proof'")" = 0
	check "the worker came back paused" bash -c "docker run --rm --network ${PROJECT}_control $CURL_IMAGE -fsS http://worker:9091/readyz | grep -q '\"status\":\"ready\"' && docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' \$(docker compose -p $PROJECT -f $HERE/compose.yml -f $HERE/compose.smoke.yml --env-file $work/previa.env ps -q worker) | grep -qx BUILDS_PAUSED=true"
	echo "     restore took ${restore_seconds}s"
	if [[ -n ${SMOKE_OLD_DUMP:-} ]]; then
		# A pg_dump (custom format) taken at an older migration: restoring it
		# must bring the schema up to this release before anything starts.
		age -r "$recipient" -o "$work/old.dump.age" "$SMOKE_OLD_DUMP"
		old_started=$(date +%s)
		check "an older-schema backup restores and migrates to this release" \
			env "${drill_env[@]}" "$HERE/restore.sh" --identity "$work/age-key.txt" --yes "$work/old.dump.age"
		check "the restored schema is current" test "$(psql "SELECT max(version_id) FROM goose_db_version WHERE is_applied")" = "$SCHEMA_VERSION"
		check "the server is ready on the upgraded data" bash -c "docker run --rm --network ${PROJECT}_control $CURL_IMAGE -fsS http://server:9090/readyz | grep -q '\"status\":\"ready\"'"
		echo "     older-schema restore and upgrade took $(($(date +%s) - old_started))s"
	fi
else
	echo "== backup and restore drill skipped: install age and age-keygen"
fi

echo "smoke test passed: $passed checks"
