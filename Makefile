.DEFAULT_GOAL := help
DB_URL ?= postgres://userapi:userapi@localhost:5432/userapi?sslmode=disable
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help run run-pg migrate test test-integration check cover lint build db-up db-down up down

help: ## Liệt kê các lệnh
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

run: ## Chạy API với storage in-memory
	go run ./cmd/userapi

run-pg: db-up migrate ## Chạy API với PostgreSQL (khởi động DB bằng docker compose)
	STORAGE=postgres DATABASE_URL="$(DB_URL)" go run ./cmd/userapi

migrate: db-up ## Áp dụng migration PostgreSQL
	DATABASE_URL="$(DB_URL)" go run ./cmd/userapi migrate

test: ## Unit test + test HTTP (không cần DB)
	go test -race ./...

test-integration: db-up ## Chạy thêm contract test trên PostgreSQL (sẽ TRUNCATE bảng users và outbox)
	TEST_DATABASE_URL="$(DB_URL)" go test -race -count=1 ./...

check: ## Mọi kiểm tra phải xanh trước khi commit: gofmt, go mod tidy, lint, test -race (gồm test PostgreSQL nếu có TEST_DATABASE_URL)
	@test -z "$$(gofmt -l .)" || (echo "Cần chạy gofmt -w trên:"; gofmt -l .; exit 1)
	go mod tidy -diff
	golangci-lint run ./...
	go test -race -count=1 ./...

cover: ## Báo cáo coverage
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint: ## golangci-lint (gồm depguard kiểm tra quy tắc phụ thuộc của kiến trúc)
	golangci-lint run ./...

build: ## Build binary vào ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/userapi ./cmd/userapi

db-up: ## Khởi động PostgreSQL
	docker compose up -d --wait db

db-down: ## Dừng PostgreSQL
	docker compose stop db

up: ## Chạy toàn bộ stack (API + DB) bằng docker compose
	docker compose up --build

down: ## Dừng và xoá container (giữ volume)
	docker compose down
