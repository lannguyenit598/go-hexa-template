.DEFAULT_GOAL := help
DB_URL ?= postgres://userapi:userapi@localhost:5432/userapi?sslmode=disable
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Start PostgreSQL through docker compose only for the default DB_URL above. Supply DB_URL
# (on the command line or in the environment) to use your database without Docker.
COMPOSE_DB := $(if $(filter file,$(origin DB_URL)),db-up)
# sqlc, pinned here for local runs and CI and built from the module proxy (no global install). With
# CGO off, sqlc uses its pure-Go (WebAssembly) PostgreSQL parser: same output, no C compiler needed.
SQLC := CGO_ENABLED=0 go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
PG_ADAPTER := internal/adapter/outbound/postgres

.PHONY: help run run-pg migrate test test-integration check cover lint build sqlc-generate sqlc-check db-up db-down up down

help: ## List commands
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

run: ## Run the API with in-memory storage
	go run ./cmd/userapi

run-pg: migrate ## Run the API with PostgreSQL (default DB_URL: start the DB through docker compose)
	STORAGE=postgres DATABASE_URL="$(DB_URL)" go run ./cmd/userapi

migrate: $(COMPOSE_DB) ## Apply PostgreSQL migrations (DB_URL=... uses another database without Docker)
	DATABASE_URL="$(DB_URL)" go run ./cmd/userapi migrate

test: ## Unit and HTTP tests (no DB required)
	go test -race ./...

test-integration: $(COMPOSE_DB) ## Also run PostgreSQL contract tests (TRUNCATEs users and outbox)
	TEST_DATABASE_URL="$(DB_URL)" go test -race -count=1 ./...

check: sqlc-check ## Required pre-commit checks: generated queries, gofmt, go mod tidy, lint, test -race (PostgreSQL tests if TEST_DATABASE_URL is set)
	@test -z "$$(gofmt -l .)" || (echo "Run gofmt -w on:"; gofmt -l .; exit 1)
	go mod tidy -diff
	golangci-lint run ./...
	go test -race -count=1 ./...

cover: ## Report coverage
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint: ## golangci-lint (including depguard architecture dependency checks)
	golangci-lint run ./...

sqlc-generate: ## Regenerate typed queries (internal/adapter/outbound/postgres/sqlcgen) from migrations and queries
	rm -rf $(PG_ADAPTER)/sqlcgen
	$(SQLC) generate

# Generates into a temporary copy and compares, so the check never rewrites the working tree and
# also catches missing, stale, and leftover (including untracked) files. No database needed.
sqlc-check: ## Fail if generated queries do not match sqlc.yaml, migrations, and queries
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	mkdir -p "$$tmp/$(PG_ADAPTER)" && cp sqlc.yaml "$$tmp/" && \
	cp -R $(PG_ADAPTER)/migrations $(PG_ADAPTER)/queries "$$tmp/$(PG_ADAPTER)/" && \
	(cd "$$tmp" && $(SQLC) generate) && \
	{ diff -r "$$tmp/$(PG_ADAPTER)/sqlcgen" $(PG_ADAPTER)/sqlcgen || \
	  { echo "Generated queries are out of date: run make sqlc-generate"; exit 1; }; }

build: ## Build the binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/userapi ./cmd/userapi

db-up: ## Start PostgreSQL
	docker compose up -d --wait db

db-down: ## Stop PostgreSQL
	docker compose stop db

up: ## Run the full stack (API + DB) through docker compose
	docker compose up --build

down: ## Stop and remove containers (preserve volumes)
	docker compose down
