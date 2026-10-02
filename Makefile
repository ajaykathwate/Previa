GO ?= go
TOOLS = $(GO) tool -modfile=tools/go.mod
ENV_FILE ?= .env

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

.PHONY: generate build test lint run-server run-worker dev migrate-up migrate-down river-migrate-up river-status

generate:
	$(TOOLS) templ generate
	@if grep -Eq '^sql: \[\][[:space:]]*$$' sqlc.yaml; then \
		echo 'sqlc: skipped (no schema or queries yet)'; \
	else \
		$(TOOLS) sqlc generate; \
	fi

build: generate
	$(GO) build ./...

test: generate
	$(GO) test ./...

lint: generate
	$(GO) vet ./...
	$(TOOLS) golangci-lint run

run-server: generate
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
