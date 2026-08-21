#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
OUTPUT=${1:-"$REPO_ROOT/benchmark/environment.txt"}
BENCHMARK_DATABASE_URL=${BENCHMARK_DATABASE_URL:-postgres://postgres:postgres@127.0.0.1:5433/ledger_benchmark?sslmode=disable}
DATA_FILE=${BENCHMARK_DATA_FILE:-"$REPO_ROOT/benchmark/data.json"}
K6_IMAGE=${K6_IMAGE:-grafana/k6:0.54.0@sha256:1f40432b1cbe7234e977f96c362c9bc550a2d2b583d014dd8669fe40d3e9e755}

mkdir -p "$(dirname -- "$OUTPUT")"
{
  echo "captured_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "uname=$(uname -srm)"
  if command -v sw_vers >/dev/null 2>&1; then
    sw_vers | sed 's/^/os_/'
  fi
  if command -v sysctl >/dev/null 2>&1; then
    sysctl -n machdep.cpu.brand_string 2>/dev/null | sed 's/^/cpu=/' || true
    sysctl -n hw.ncpu 2>/dev/null | sed 's/^/host_logical_cpu=/' || true
    sysctl -n hw.memsize 2>/dev/null | sed 's/^/host_memory_bytes=/' || true
  fi
  echo "go=$(go version 2>&1)"
  echo "make=$(make --version 2>&1 | sed -n '1p')"
  if command -v k6 >/dev/null 2>&1; then
    echo "k6=$(k6 version 2>&1 | sed -n '1p')"
  else
    echo "k6_image=$K6_IMAGE"
    docker image inspect "$K6_IMAGE" --format 'k6_image_id={{.Id}} k6_repo_digests={{join .RepoDigests ","}}' 2>/dev/null || true
  fi
  if command -v docker >/dev/null 2>&1; then
    echo "docker=$(docker version --format '{{.Client.Version}}/{{.Server.Version}}' 2>/dev/null || true)"
    echo "docker_compose=$(docker compose version 2>/dev/null || true)"
    docker info --format 'docker_cpus={{.NCPU}} docker_memory_bytes={{.MemTotal}} docker_arch={{.Architecture}}' 2>/dev/null || true
    docker inspect doubleentryledger-postgres-benchmark-1 --format 'postgres_image={{.Config.Image}} postgres_image_id={{.Image}}' 2>/dev/null || true
    docker inspect doubleentryledger-nats-benchmark-1 --format 'nats_image={{.Config.Image}} nats_image_id={{.Image}}' 2>/dev/null || true
  fi
  echo "database_host=127.0.0.1:5433"
  echo "database_name=ledger_benchmark"
  echo "db_max_open_conns=${DB_MAX_OPEN_CONNS:-90}"
  echo "db_max_idle_conns=${DB_MAX_IDLE_CONNS:-90}"
  echo "db_conn_max_lifetime=${DB_CONN_MAX_LIFETIME:-30m}"
  echo "db_conn_max_idle_time=${DB_CONN_MAX_IDLE_TIME:-5m}"
  echo "http_request_log=${HTTP_REQUEST_LOG:-false}"
  echo "worker_batch_size=${WORKER_BATCH_SIZE:-100}"
  echo "worker_idle_delay=${WORKER_IDLE_DELAY:-10ms}"
  echo "transfer_workers=${TRANSFER_WORKERS:-16}"
  echo "publisher_workers=${PUBLISHER_WORKERS:-16}"
  echo "aggregate_workers=${AGGREGATE_WORKERS:-4}"
  echo "aggregate_fetch_batch=${AGGREGATE_FETCH_BATCH:-64}"
  echo "posting_batch_size=${POSTING_BATCH_SIZE:-32}"
  echo "posting_batch_wait=${POSTING_BATCH_WAIT:-500us}"
  echo "posting_batch_workers=${POSTING_BATCH_WORKERS:-4}"
  echo "benchmark_stage=${BENCHMARK_STAGE:-unlabeled}"
  echo "benchmark_predecessor=${BENCHMARK_PREDECESSOR:-none}"
  echo "benchmark_change_under_test=${BENCHMARK_CHANGE_UNDER_TEST:-unspecified}"
  echo "benchmark_objective=${BENCHMARK_OBJECTIVE:-measure the configured workload}"
  echo "target_tps=${TARGET_TPS:-10000}"
  echo "warmup_tps=${WARMUP_TPS:-100}"
  echo "warmup_duration=${WARMUP_DURATION:-15s}"
  echo "warmup_graceful_stop=${WARMUP_GRACEFUL_STOP:-20s}"
  echo "warmup_settle_duration=${WARMUP_SETTLE_DURATION:-2s}"
  echo "measurement_duration=${DURATION:-30s}"
  echo "measurement_graceful_stop=${GRACEFUL_STOP:-30s}"
  echo "pre_allocated_vus=${PRE_ALLOCATED_VUS:-auto}"
  echo "max_vus=${MAX_VUS:-auto}"
  echo "transfer_completion_poll=${POLL_TRANSFER_COMPLETION:-true}"
  echo "poll_interval_seconds=${POLL_INTERVAL_SECONDS:-0.01}"
  if [ -f "$DATA_FILE" ]; then
    if command -v jq >/dev/null 2>&1; then
      jq -r '.dataset | to_entries[] | "dataset_\(.key)=\(.value)"' "$DATA_FILE"
    else
      echo "dataset_file=$DATA_FILE"
    fi
  fi
  echo "git_head=$(git -C "$REPO_ROOT" rev-parse --verify HEAD 2>/dev/null || echo unborn)"
  echo
  echo "[postgres_version]"
  if command -v psql >/dev/null 2>&1; then
    psql "$BENCHMARK_DATABASE_URL" -X -Atc 'SELECT version()' 2>&1 || true
    echo
    echo "[postgres_settings]"
    psql "$BENCHMARK_DATABASE_URL" -X -P pager=off -c "SELECT name,setting,unit,source FROM pg_settings WHERE name IN ('max_connections','shared_buffers','effective_cache_size','work_mem','maintenance_work_mem','wal_level','synchronous_commit','max_wal_size','checkpoint_timeout','random_page_cost','effective_io_concurrency','jit') ORDER BY name" 2>&1 || true
    echo
    echo "[dataset_sizes]"
    psql "$BENCHMARK_DATABASE_URL" -X -P pager=off -c "SELECT current_database() database,pg_database_size(current_database()) bytes" -c "SELECT relname,n_live_tup,pg_total_relation_size(relid) bytes FROM pg_stat_user_tables ORDER BY relname" 2>&1 || true
  fi
} > "$OUTPUT"
