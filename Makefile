GO ?= go
TOOLS = $(GO) tool -modfile=tools/go.mod
ENV_FILE ?= .env
TAILWIND = ./bin/tailwindcss-v4.1.18

# Load trusted shell assignments for local runs, preserving exported overrides.
# export -p emits shell-quoted declarations; restore those without logging values.
# A missing default .env allows deployments to use only their process environment.
define load_run_env
set -eu; \
previa_exports=$$(export -p); \
previa_env_file="$(ENV_FILE)"; \
case "$$previa_env_file" in /*|./*|../*) ;; *) previa_env_file="./$$previa_env_file" ;; esac; \
if [ -f "$$previa_env_file" ]; then \
	set -a; . "$$previa_env_file"; set +a; \
elif [ "$(ENV_FILE)" != '.env' ]; then \
	echo 'Environment file not found' >&2; exit 1; \
fi; \
eval "$$previa_exports";
endef

.PHONY: generate templates watch-templates assets build test test-browser browser-install test-github-app test-source test-reporting secrets-reencrypt egress-image test-builder test-runtime test-routing test-housekeeping test-e2e router-up router-down route-sync housekeeping-dry-run lint run-server run-worker dev migrate-up migrate-down river-migrate-up river-status

$(TAILWIND): deploy/install-tailwind.sh
	sh deploy/install-tailwind.sh "$@"

assets: $(TAILWIND)
	@set -eu; mkdir -p web/static/css; \
	previa_css=$$(mktemp web/static/css/.app.css.XXXXXX); \
	trap 'rm -f "$$previa_css"' EXIT HUP INT TERM; \
	$(TAILWIND) -i web/styles/app.css -o "$$previa_css" --minify; \
	if ! cmp -s "$$previa_css" web/static/css/app.css; then \
		mv "$$previa_css" web/static/css/app.css; \
	fi

# Templates must exist before Go/editor tooling can resolve component functions.
templates:
	$(TOOLS) templ generate -path web

# Optional template-only watcher; does not start the server or require .env.
watch-templates:
	$(TOOLS) air -c .air.templ.toml

generate: templates
	@if grep -Eq '^sql: \[\][[:space:]]*$$' sqlc.yaml; then \
		echo 'sqlc: skipped (no schema or queries yet)'; \
	else \
		$(TOOLS) sqlc generate; \
	fi

build: assets generate
	$(GO) build ./...

test: assets generate
	$(GO) test ./...

# Optional browser tooling is isolated from normal Go/application commands.
browser-install:
	npm ci --prefix web/tests/browser
	npm exec --prefix web/tests/browser -- playwright install chromium

test-browser: assets generate
	RUN_BROWSER_TESTS=1 $(GO) test -count=1 -v ./web/tests/browser

# Opt-in real GitHub proof; reads credentials from .env, never CLI flags.
test-github-app:
	@$(load_run_env) if [ -z "$${TEST_GITHUB_INSTALLATION_ID:-}" ] || [ -z "$${TEST_GITHUB_REPO_ID:-}" ]; then \
		echo 'Set TEST_GITHUB_INSTALLATION_ID and TEST_GITHUB_REPO_ID to real numeric GitHub IDs' >&2; exit 1; \
	fi; \
	$(GO) test -count=1 -v -run '^TestRealInstallationRepositoryAccess$$' ./internal/platform/githubapp

# Opt-in real source check: verifies one PR's archive identity, extraction and
# config. Reads App credentials from .env, never CLI flags; writes nothing to GitHub.
test-source:
	@$(load_run_env) if [ -z "$${TEST_GITHUB_INSTALLATION_ID:-}" ] || [ -z "$${TEST_GITHUB_REPO_ID:-}" ] || [ -z "$${TEST_GITHUB_PR_NUMBER:-}" ]; then \
		echo 'Set TEST_GITHUB_INSTALLATION_ID, TEST_GITHUB_REPO_ID and TEST_GITHUB_PR_NUMBER to real numeric GitHub values' >&2; exit 1; \
	fi; \
	$(GO) test -count=1 -v -run '^TestRealSourcePrepare$$' ./internal/source

# Opt-in live PR feedback proof on an authorized disposable PR: writes one
# living comment and check runs named "Previa preview (live proof)" to that PR,
# then deletes the comment and completes open runs. Uses a throwaway schema.
test-reporting:
	@$(load_run_env) if [ -z "$${TEST_GITHUB_INSTALLATION_ID:-}" ] || [ -z "$${TEST_GITHUB_REPO_ID:-}" ] || [ -z "$${TEST_GITHUB_PR_NUMBER:-}" ]; then \
		echo 'Set TEST_GITHUB_INSTALLATION_ID, TEST_GITHUB_REPO_ID and TEST_GITHUB_PR_NUMBER to a disposable PR you may write to' >&2; exit 1; \
	fi; \
	export TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$$DATABASE_URL}"; \
	TEST_REPORTING=1 $(GO) test -count=1 -v -timeout 10m -run '^TestRealReportingOnADisposablePR$$' ./internal/reporting

# Moves repository secrets onto SECRETS_ACTIVE_KEY_VERSION; prints counts only.
secrets-reencrypt:
	@$(load_run_env) $(GO) run ./cmd/secrets-reencrypt

# Image of the egress proxy and readiness prober, the only route out of build
# and preview networks: a static binary for the Docker host's architecture in
# an otherwise empty image.
egress-image:
	@$(load_run_env) previa_dir=$$(mktemp -d); trap 'rm -rf "$$previa_dir"' EXIT; \
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(docker version --format '{{.Server.Arch}}') \
		$(GO) build -trimpath -ldflags='-s -w' -o "$$previa_dir/previa-egress" ./cmd/egress; \
	cp deploy/egress/Dockerfile "$$previa_dir/"; \
	docker build --quiet -t "$${EGRESS_IMAGE:-previa-egress:dev}" "$$previa_dir"

# Opt-in real builds on the local Docker Engine: isolation probes, limits,
# caching and cleanup. Needs Docker and internet access for base images.
test-builder: egress-image
	@$(load_run_env) TEST_BUILDER=1 $(GO) test -count=1 -v -timeout 30m -run '^TestRealBuilder' ./internal/builds

# Opt-in real previews on the local Docker Engine: hardening, readiness, crash
# handling, secrets withholding, network denial, capacity and cleanup.
test-runtime: egress-image
	@$(load_run_env) export TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$$DATABASE_URL}"; \
	TEST_RUNTIME=1 $(GO) test -count=1 -v -timeout 30m -run '^TestRealRuntime' ./internal/runtime

# Opt-in real routing, publication and teardown: Traefik with a wildcard
# certificate from a local Pebble ACME server over DNS-01, real previews,
# swaps, removals, revocation and cleanup against a throwaway database schema.
test-routing: egress-image
	@$(load_run_env) export TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$$DATABASE_URL}"; \
	TEST_ROUTING=1 $(GO) test -count=1 -v -timeout 30m -run '^TestRealRouting' ./internal/routing

# Opt-in real end-to-end proof of the worker's own pipeline on the local
# Docker Engine: a fake GitHub (TLS), real BuildKit builds, hardened previews,
# Traefik routing, PR reports, logs and teardown, on a throwaway schema.
test-e2e: egress-image
	@$(load_run_env) export TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$$DATABASE_URL}"; \
	TEST_E2E=1 $(GO) test -count=1 -v -timeout 25m -run '^TestRealEndToEnd$$' ./internal/app

# Opt-in real housekeeping on the local Docker Engine: grace periods, dry run,
# orphans of known and unknown PRs, other instances left alone, and an
# emergency prune of caches and untagged images, against a throwaway schema.
test-housekeeping: egress-image
	@$(load_run_env) export TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$$DATABASE_URL}"; \
	TEST_HOUSEKEEPING=1 $(GO) test -count=1 -v -timeout 15m -run '^TestRealHousekeeping' ./internal/housekeeping

# The router (Traefik) for local/staging use. Reads routing and ACME settings
# from .env; the DNS credential stays in its own mode-600 file.
router-up:
	@$(load_run_env) for v in PREVIEW_BASE_DOMAIN ROUTER_DYNAMIC_DIR ACME_EMAIL ACME_DNS_PROVIDER ACME_DNS_CREDENTIAL_VARIABLE ACME_DNS_CREDENTIAL_FILE; do \
		eval "val=\$${$$v:-}"; [ -n "$$val" ] || { echo "Set $$v in .env (see docs/11-routing-and-https.md)" >&2; exit 1; }; \
	done; \
	case "$$ROUTER_DYNAMIC_DIR" in /mnt/*) echo 'ROUTER_DYNAMIC_DIR must be on a Linux filesystem: Traefik cannot watch /mnt/ paths' >&2; exit 1;; esac; \
	[ -f "$$ACME_DNS_CREDENTIAL_FILE" ] || { echo 'ACME_DNS_CREDENTIAL_FILE does not exist' >&2; exit 1; }; \
	[ "$$(stat -c %a "$$ACME_DNS_CREDENTIAL_FILE")" = 600 ] || { echo 'chmod 600 the DNS credential file first' >&2; exit 1; }; \
	mkdir -p "$$ROUTER_DYNAMIC_DIR"; chmod 750 "$$ROUTER_DYNAMIC_DIR"; \
	$(GO) run ./cmd/route-sync; \
	docker compose -f deploy/traefik/compose.yml up -d

# Writes the accepted route table from PostgreSQL to ROUTER_DYNAMIC_DIR (the
# worker's route controller does this too once execution is wired).
route-sync:
	@$(load_run_env) $(GO) run ./cmd/route-sync

# Housekeeping diagnostics: one inventory, expiry and prune pass as a dry run
# against the configured database and Docker. Logs every decision; changes
# nothing. WORKSPACES=/abs/path also inspects a source workspace directory.
housekeeping-dry-run:
	@$(load_run_env) $(GO) run ./cmd/housekeeping $${WORKSPACES:+-workspaces "$$WORKSPACES"}

router-down:
	@$(load_run_env) docker compose -f deploy/traefik/compose.yml down

lint: assets generate
	$(GO) vet ./...
	$(TOOLS) golangci-lint run

run-server: assets generate
	@$(load_run_env) exec $(GO) run ./cmd/server

dev:
	$(TOOLS) air -c .air.toml

run-worker: generate
	@$(load_run_env) exec $(GO) run ./cmd/worker

# DATABASE_URL from the environment takes precedence over the local env file.
# Fall back to .env.example for a fresh local checkout. Files use shell syntax.
# GOOSE_DBSTRING keeps credentials out of command-line arguments.
migrate-up migrate-down:
	@set -eu; \
	if [ -z "$${DATABASE_URL:-}" ]; then \
		if [ -f "$(ENV_FILE)" ]; then . "./$(ENV_FILE)"; \
		elif [ "$(ENV_FILE)" = '.env' ]; then . ./.env.example; \
		else echo 'Environment file not found' >&2; exit 1; fi; \
	fi; \
	: "$${DATABASE_URL:?DATABASE_URL must be set}"; \
	GOOSE_DRIVER=postgres GOOSE_DBSTRING="$$DATABASE_URL" $(TOOLS) goose -env=none -dir db/migrations $(patsubst migrate-%,%,$@)
	@if [ '$@' = migrate-up ]; then $(MAKE) river-migrate-up; fi

# River owns its schema/history. The adapter reads DATABASE_URL from the environment.
river-migrate-up river-status:
	@set -eu; \
	if [ -z "$${DATABASE_URL:-}" ]; then \
		if [ -f "$(ENV_FILE)" ]; then . "./$(ENV_FILE)"; \
		elif [ "$(ENV_FILE)" = '.env' ]; then . ./.env.example; \
		else echo 'Environment file not found' >&2; exit 1; fi; \
	fi; \
	: "$${DATABASE_URL:?DATABASE_URL must be set}"; \
	export DATABASE_URL; \
	$(GO) run ./cmd/river-migrate $(if $(filter river-status,$@),status,up)

# Signs a local fixture in memory; secret is never a command argument.
.PHONY: test-webhook
test-webhook:
	@$(load_run_env) python3 deploy/test-webhook.py
