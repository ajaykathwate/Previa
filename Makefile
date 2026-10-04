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

.PHONY: generate templates watch-templates assets build test test-browser browser-install test-github-app test-source secrets-reencrypt build-egress-image test-builder lint run-server run-worker dev migrate-up migrate-down river-migrate-up river-status

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

# Moves repository secrets onto SECRETS_ACTIVE_KEY_VERSION; prints counts only.
secrets-reencrypt:
	@$(load_run_env) $(GO) run ./cmd/secrets-reencrypt

# Image of the proxy that is the only route out of build networks: a static
# binary for the Docker host's architecture in an otherwise empty image.
build-egress-image:
	@$(load_run_env) previa_dir=$$(mktemp -d); trap 'rm -rf "$$previa_dir"' EXIT; \
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(docker version --format '{{.Server.Arch}}') \
		$(GO) build -trimpath -ldflags='-s -w' -o "$$previa_dir/build-egress" ./cmd/build-egress; \
	cp deploy/build-egress/Dockerfile "$$previa_dir/"; \
	docker build --quiet -t "$${BUILD_EGRESS_IMAGE:-previa-build-egress:dev}" "$$previa_dir"

# Opt-in real builds on the local Docker Engine: isolation probes, limits,
# caching and cleanup. Needs Docker and internet access for base images.
test-builder: build-egress-image
	@$(load_run_env) TEST_BUILDER=1 $(GO) test -count=1 -v -timeout 30m -run '^TestRealBuilder' ./internal/builds

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
