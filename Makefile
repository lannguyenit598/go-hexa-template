.DEFAULT_GOAL := help
DB_URL ?= postgres://userapi:userapi@localhost:5432/userapi?sslmode=disable
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Start PostgreSQL through docker compose only for the default DB_URL above. Supply DB_URL
# (on the command line or in the environment) to use your database without Docker.
COMPOSE_DB := $(if $(filter file,$(origin DB_URL)),db-up)

.PHONY: help run run-pg migrate test test-integration check cover lint build db-up db-down up down

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

check: ## Required pre-commit checks: gofmt, go mod tidy, lint, test -race (PostgreSQL tests if TEST_DATABASE_URL is set)
	@test -z "$$(gofmt -l .)" || (echo "Run gofmt -w on:"; gofmt -l .; exit 1)
	go mod tidy -diff
	golangci-lint run ./...
	go test -race -count=1 ./...

cover: ## Report coverage
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint: ## golangci-lint (including depguard architecture dependency checks)
	golangci-lint run ./...

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
