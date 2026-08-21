#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
OUTPUT=${1:-"$REPO_ROOT/benchmark/database-state.txt"}
BENCHMARK_DATABASE_URL=${BENCHMARK_DATABASE_URL:-postgres://postgres:postgres@127.0.0.1:5433/ledger_benchmark?sslmode=disable}

mkdir -p "$(dirname -- "$OUTPUT")"
QUERIES=(
  "SELECT event_type,status,COUNT(*) count FROM outbox_events GROUP BY event_type,status ORDER BY event_type,status"
  "SELECT status,COUNT(*) count FROM sagas GROUP BY status ORDER BY status"
  "SELECT COUNT(*) processed_events FROM processed_events"
  "SELECT COUNT(*) daily_account_aggregate_rows FROM daily_account_aggregates"
  "SELECT COUNT(*) daily_ledger_aggregate_rows FROM daily_ledger_aggregates"
)

run_query() {
  if command -v psql >/dev/null 2>&1; then
    psql "$BENCHMARK_DATABASE_URL" -X -v ON_ERROR_STOP=1 -P pager=off -c "$1"
  else
    docker compose -f "$REPO_ROOT/docker-compose.yml" exec -T postgres-benchmark \
      psql -U postgres -d ledger_benchmark -X -v ON_ERROR_STOP=1 -P pager=off -c "$1"
  fi
}

{
  for query in "${QUERIES[@]}"; do
    run_query "$query"
  done
} > "$OUTPUT"
