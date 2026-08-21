-include .env
export

DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/ledger?sslmode=disable
BENCHMARK_DATABASE_URL ?= postgres://postgres:postgres@127.0.0.1:5433/ledger_benchmark?sslmode=disable
BENCHMARK_NATS_URL ?= nats://127.0.0.1:4223
BENCHMARK_TARGET_TPS ?= 10000
BENCHMARK_STAGE ?= unlabeled
BENCHMARK_PREDECESSOR ?= none
BENCHMARK_CHANGE_UNDER_TEST ?= unspecified
BENCHMARK_OBJECTIVE ?= measure the configured workload

.PHONY: build db-up db-down run test test-postgres migrate benchmark-db-up benchmark-smoke benchmark benchmark-profile

build:
	go build ./...

db-up:
	docker compose up -d postgres nats

db-down:
	docker compose down

run:
	go run ./cmd/ledger

test:
	DATABASE_URL= go test ./...

test-postgres:
	DATABASE_URL="$(DATABASE_URL)" go test ./... -count=1

migrate:
	migrate -path ./migrations -database "$(DATABASE_URL)" up

benchmark-db-up:
	docker compose up -d --wait postgres-benchmark nats-benchmark

benchmark-smoke: benchmark-db-up
	BENCHMARK_DATABASE_URL="$(BENCHMARK_DATABASE_URL)" NATS_URL="$(BENCHMARK_NATS_URL)" TARGET_TPS=100 WARMUP_DURATION=5s DURATION=10s PROFILE_SECONDS=10 BENCHMARK_STAGE="$(BENCHMARK_STAGE)" BENCHMARK_PREDECESSOR="$(BENCHMARK_PREDECESSOR)" BENCHMARK_CHANGE_UNDER_TEST="$(BENCHMARK_CHANGE_UNDER_TEST)" BENCHMARK_OBJECTIVE="$(BENCHMARK_OBJECTIVE)" ./benchmark/scripts/run.sh

benchmark: benchmark-db-up
	BENCHMARK_DATABASE_URL="$(BENCHMARK_DATABASE_URL)" NATS_URL="$(BENCHMARK_NATS_URL)" TARGET_TPS="$(BENCHMARK_TARGET_TPS)" BENCHMARK_STAGE="$(BENCHMARK_STAGE)" BENCHMARK_PREDECESSOR="$(BENCHMARK_PREDECESSOR)" BENCHMARK_CHANGE_UNDER_TEST="$(BENCHMARK_CHANGE_UNDER_TEST)" BENCHMARK_OBJECTIVE="$(BENCHMARK_OBJECTIVE)" ./benchmark/scripts/run.sh

benchmark-profile: benchmark-db-up
	BENCHMARK_DATABASE_URL="$(BENCHMARK_DATABASE_URL)" NATS_URL="$(BENCHMARK_NATS_URL)" TARGET_TPS="$${TARGET_TPS:-100}" CAPTURE_PROFILES=1 BENCHMARK_STAGE="$(BENCHMARK_STAGE)" BENCHMARK_PREDECESSOR="$(BENCHMARK_PREDECESSOR)" BENCHMARK_CHANGE_UNDER_TEST="$(BENCHMARK_CHANGE_UNDER_TEST)" BENCHMARK_OBJECTIVE="$(BENCHMARK_OBJECTIVE)" ./benchmark/scripts/run.sh
