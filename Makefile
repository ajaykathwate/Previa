GO ?= go
TOOLS = $(GO) tool -modfile=tools/go.mod
ENV_FILE ?= .env

.PHONY: generate build test lint run-server run-worker dev migrate-up migrate-down

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
	$(GO) run ./cmd/server

dev:
	$(TOOLS) air -c .air.toml

run-worker:
	$(GO) run ./cmd/worker

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
