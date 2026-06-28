-include .env
export

DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/ledger?sslmode=disable

.PHONY: db-up db-down run test test-postgres migrate

build:
	go build ./...

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

run:
	go run ./cmd/ledger

test:
	DATABASE_URL= go test ./...

test-postgres:
	go test ./internal/ledger -run TestPostgresStore -count=1 -v

migrate:
	migrate -path ./migrations -database "$(DATABASE_URL)" up
