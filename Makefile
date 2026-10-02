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

.PHONY: generate templates watch-templates assets build test lint run-server run-worker dev migrate-up migrate-down river-migrate-up river-status

$(TAILWIND): deploy/install-tailwind.sh
	sh deploy/install-tailwind.sh "$@"

assets: $(TAILWIND)
	@set -eu; mkdir -p web/static/css; \
	previa_css=$$(mktemp web/static/css/app.css.XXXXXX); \
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
