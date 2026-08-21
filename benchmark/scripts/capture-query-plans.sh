#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
OUTPUT=${1:-"$REPO_ROOT/benchmark/query-plans.txt"}
BENCHMARK_DATABASE_URL=${BENCHMARK_DATABASE_URL:-postgres://postgres:postgres@127.0.0.1:5433/ledger_benchmark?sslmode=disable}

mkdir -p "$(dirname -- "$OUTPUT")"
if command -v psql >/dev/null 2>&1; then
  psql "$BENCHMARK_DATABASE_URL" -X -v ON_ERROR_STOP=1 -f "$REPO_ROOT/benchmark/sql/query-plans.sql" > "$OUTPUT"
else
  docker compose -f "$REPO_ROOT/docker-compose.yml" exec -T postgres-benchmark \
    psql -U postgres -d ledger_benchmark -X -v ON_ERROR_STOP=1 \
    < "$REPO_ROOT/benchmark/sql/query-plans.sql" > "$OUTPUT"
fi
