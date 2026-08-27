#!/usr/bin/env bash
set -uo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
cd "$REPO_ROOT"

BENCHMARK_DATABASE_URL=${BENCHMARK_DATABASE_URL:-postgres://postgres:postgres@127.0.0.1:5433/ledger_benchmark?sslmode=disable}
NATS_URL=${NATS_URL:-nats://127.0.0.1:4223}
BASE_URL=${BASE_URL:-http://127.0.0.1:3000}
PPROF_URL=${PPROF_URL:-http://127.0.0.1:6060}
TARGET_TPS=${TARGET_TPS:-10000}
if [ -z "${WARMUP_TPS:-}" ]; then
  if [ "$TARGET_TPS" -lt 100 ]; then
    WARMUP_TPS=$TARGET_TPS
  else
    WARMUP_TPS=100
  fi
fi
WARMUP_DURATION=${WARMUP_DURATION:-15s}
WARMUP_GRACEFUL_STOP=${WARMUP_GRACEFUL_STOP:-20s}
WARMUP_SETTLE_DURATION=${WARMUP_SETTLE_DURATION:-2s}
DURATION=${DURATION:-30s}
GRACEFUL_STOP=${GRACEFUL_STOP:-30s}
POLL_TRANSFER_COMPLETION=${POLL_TRANSFER_COMPLETION:-true}
POLL_INTERVAL_SECONDS=${POLL_INTERVAL_SECONDS:-0.01}
BENCHMARK_ACCOUNT_PAIRS=${BENCHMARK_ACCOUNT_PAIRS:-512}
BENCHMARK_HISTORY_TRANSACTIONS=${BENCHMARK_HISTORY_TRANSACTIONS:-100000}
DB_MAX_OPEN_CONNS=${DB_MAX_OPEN_CONNS:-90}
DB_MAX_IDLE_CONNS=${DB_MAX_IDLE_CONNS:-90}
DB_CONN_MAX_LIFETIME=${DB_CONN_MAX_LIFETIME:-30m}
DB_CONN_MAX_IDLE_TIME=${DB_CONN_MAX_IDLE_TIME:-5m}
WORKER_BATCH_SIZE=${WORKER_BATCH_SIZE:-100}
WORKER_IDLE_DELAY=${WORKER_IDLE_DELAY:-10ms}
TRANSFER_WORKERS=${TRANSFER_WORKERS:-1}
PUBLISHER_WORKERS=${PUBLISHER_WORKERS:-16}
PUBLISH_ASYNC_MAX_PENDING=${PUBLISH_ASYNC_MAX_PENDING:-256}
AGGREGATE_WORKERS=${AGGREGATE_WORKERS:-4}
AGGREGATE_FETCH_BATCH=${AGGREGATE_FETCH_BATCH:-64}
POSTING_BATCH_SIZE=${POSTING_BATCH_SIZE:-32}
POSTING_BATCH_WAIT=${POSTING_BATCH_WAIT:-500us}
POSTING_BATCH_WORKERS=${POSTING_BATCH_WORKERS:-4}
BENCHMARK_STAGE=${BENCHMARK_STAGE:-unlabeled}
BENCHMARK_PREDECESSOR=${BENCHMARK_PREDECESSOR:-none}
BENCHMARK_CHANGE_UNDER_TEST=${BENCHMARK_CHANGE_UNDER_TEST:-unspecified}
BENCHMARK_OBJECTIVE=${BENCHMARK_OBJECTIVE:-measure the configured workload}
CAPTURE_PROFILES=${CAPTURE_PROFILES:-1}
PROFILE_SECONDS=${PROFILE_SECONDS:-30}
K6_IMAGE=${K6_IMAGE:-grafana/k6:0.54.0@sha256:1f40432b1cbe7234e977f96c362c9bc550a2d2b583d014dd8669fe40d3e9e755}
GOCACHE=${GOCACHE:-/tmp/double-entry-ledger-go-cache}
RUN_STAMP=${RUN_STAMP:-$(date -u +%Y-%m-%dT%H%M%SZ)}
RESULT_DIR=${RESULT_DIR:-$REPO_ROOT/benchmark/results/$RUN_STAMP}
BENCHMARK_DATA_FILE=${BENCHMARK_DATA_FILE:-$REPO_ROOT/benchmark/data.json}
START_SERVER=${START_SERVER:-1}

mkdir -p "$RESULT_DIR"
RESULT_DIR=$(CDPATH= cd -- "$RESULT_DIR" && pwd)
export BENCHMARK_STAGE BENCHMARK_PREDECESSOR BENCHMARK_CHANGE_UNDER_TEST BENCHMARK_OBJECTIVE
"$SCRIPT_DIR/capture-run-metadata.sh" "$RESULT_DIR/run-metadata.json"
TMP_RUN_DIR=$(mktemp -d "${TMPDIR:-/tmp}/double-entry-ledger-benchmark.XXXXXX")
SERVER_PID=
PROFILE_PID=

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [ -n "$PROFILE_PID" ]; then
    wait "$PROFILE_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_RUN_DIR"
}
trap cleanup EXIT INT TERM

if [ "$START_SERVER" = "1" ] && curl --silent --output /dev/null --max-time 1 "$BASE_URL/health"; then
  echo "refusing to benchmark: an HTTP server is already responding at $BASE_URL" >&2
  exit 1
fi

echo "Seeding isolated benchmark database..."
if ! env BENCHMARK_DATABASE_URL="$BENCHMARK_DATABASE_URL" NATS_URL="$NATS_URL" \
  BENCHMARK_ACCOUNT_PAIRS="$BENCHMARK_ACCOUNT_PAIRS" \
  BENCHMARK_HISTORY_TRANSACTIONS="$BENCHMARK_HISTORY_TRANSACTIONS" \
  GOCACHE="$GOCACHE" go run ./cmd/benchmark-seed -output "$BENCHMARK_DATA_FILE"; then
  echo "seed failed; run 'make benchmark-db-up' and verify the dedicated database" >&2
  exit 1
fi
cp "$BENCHMARK_DATA_FILE" "$RESULT_DIR/seed-data.json"

LEDGER_BINARY="$TMP_RUN_DIR/ledger"
GOCACHE="$GOCACHE" go build -o "$LEDGER_BINARY" ./cmd/ledger

if [ "$START_SERVER" = "1" ]; then
  env DATABASE_URL="$BENCHMARK_DATABASE_URL" NATS_URL="$NATS_URL" \
    HTTP_ADDR=127.0.0.1:3000 PPROF_ADDR=127.0.0.1:6060 HTTP_REQUEST_LOG=false \
    DB_MAX_OPEN_CONNS="$DB_MAX_OPEN_CONNS" DB_MAX_IDLE_CONNS="$DB_MAX_IDLE_CONNS" \
    DB_CONN_MAX_LIFETIME="$DB_CONN_MAX_LIFETIME" DB_CONN_MAX_IDLE_TIME="$DB_CONN_MAX_IDLE_TIME" \
    WORKER_BATCH_SIZE="$WORKER_BATCH_SIZE" WORKER_IDLE_DELAY="$WORKER_IDLE_DELAY" \
    TRANSFER_WORKERS="$TRANSFER_WORKERS" PUBLISHER_WORKERS="$PUBLISHER_WORKERS" \
    PUBLISH_ASYNC_MAX_PENDING="$PUBLISH_ASYNC_MAX_PENDING" \
    AGGREGATE_WORKERS="$AGGREGATE_WORKERS" AGGREGATE_FETCH_BATCH="$AGGREGATE_FETCH_BATCH" \
    POSTING_BATCH_SIZE="$POSTING_BATCH_SIZE" POSTING_BATCH_WAIT="$POSTING_BATCH_WAIT" \
    POSTING_BATCH_WORKERS="$POSTING_BATCH_WORKERS" \
    "$LEDGER_BINARY" > "$RESULT_DIR/server.log" 2>&1 &
  SERVER_PID=$!
fi

ready=0
for _ in $(seq 1 120); do
  if [ -n "$SERVER_PID" ] && ! kill -0 "$SERVER_PID" 2>/dev/null; then
    break
  fi
  if curl --silent --fail "$BASE_URL/health" >/dev/null 2>&1; then
    if [ -z "$SERVER_PID" ] || kill -0 "$SERVER_PID" 2>/dev/null; then
      ready=1
      break
    fi
  fi
  sleep 0.5
done
if [ "$ready" != "1" ]; then
  echo "server did not become healthy at $BASE_URL" >&2
  exit 1
fi

PRE_ALLOCATED_VUS=${PRE_ALLOCATED_VUS:-}
MAX_VUS=${MAX_VUS:-}
if [ "$TARGET_TPS" -ge 1000 ]; then
  PRE_ALLOCATED_VUS=${PRE_ALLOCATED_VUS:-256}
  MAX_VUS=${MAX_VUS:-512}
fi

if ! command -v k6 >/dev/null 2>&1; then
  if ! docker image inspect "$K6_IMAGE" >/dev/null 2>&1; then
    docker pull "$K6_IMAGE"
  fi
fi

export BENCHMARK_DATABASE_URL BENCHMARK_DATA_FILE TARGET_TPS WARMUP_TPS
export WARMUP_DURATION WARMUP_GRACEFUL_STOP WARMUP_SETTLE_DURATION DURATION GRACEFUL_STOP
export DB_MAX_OPEN_CONNS DB_MAX_IDLE_CONNS DB_CONN_MAX_LIFETIME DB_CONN_MAX_IDLE_TIME
export WORKER_BATCH_SIZE WORKER_IDLE_DELAY TRANSFER_WORKERS PUBLISHER_WORKERS
export PUBLISH_ASYNC_MAX_PENDING
export AGGREGATE_WORKERS AGGREGATE_FETCH_BATCH POLL_TRANSFER_COMPLETION K6_IMAGE
export POSTING_BATCH_SIZE POSTING_BATCH_WAIT POSTING_BATCH_WORKERS
export POLL_INTERVAL_SECONDS PRE_ALLOCATED_VUS MAX_VUS
"$SCRIPT_DIR/capture-environment.sh" "$RESULT_DIR/environment.txt"

if [ "$CAPTURE_PROFILES" = "1" ]; then
  PROFILE_DELAY_SECONDS=$(python3 - "$WARMUP_DURATION" "$WARMUP_GRACEFUL_STOP" "$WARMUP_SETTLE_DURATION" <<'PY'
import re, sys
scale = {'ms': .001, 's': 1, 'm': 60, 'h': 3600}
def seconds(raw):
    return sum(float(value) * scale[unit] for value, unit in re.findall(r'(\d+(?:\.\d+)?)(ms|s|m|h)', raw.replace(' ', '')))
print(sum(seconds(value) for value in sys.argv[1:]))
PY
)
  PPROF_URL="$PPROF_URL" PROFILE_SECONDS="$PROFILE_SECONDS" PROFILE_DELAY_SECONDS="$PROFILE_DELAY_SECONDS" GOCACHE="$GOCACHE" \
    "$SCRIPT_DIR/capture-profiles.sh" "$RESULT_DIR" "$LEDGER_BINARY" \
    > "$RESULT_DIR/profile-capture.log" 2>&1 &
  PROFILE_PID=$!
fi

K6_SUMMARY="$RESULT_DIR/k6-summary.json"
K6_SAMPLES="$RESULT_DIR/k6-samples.json"
K6_RUN_ID=${BENCHMARK_RUN_ID:-$RUN_STAMP}
K6_STATUS=0
if command -v k6 >/dev/null 2>&1; then
  K6_COMMAND=(
    k6 run
    -e "BASE_URL=$BASE_URL"
    -e "BENCHMARK_DATA_FILE=$BENCHMARK_DATA_FILE"
    -e "TARGET_TPS=$TARGET_TPS"
    -e "WARMUP_TPS=$WARMUP_TPS"
    -e "WARMUP_DURATION=$WARMUP_DURATION"
    -e "WARMUP_GRACEFUL_STOP=$WARMUP_GRACEFUL_STOP"
    -e "WARMUP_SETTLE_DURATION=$WARMUP_SETTLE_DURATION"
    -e "DURATION=$DURATION"
    -e "GRACEFUL_STOP=$GRACEFUL_STOP"
    -e "POLL_TRANSFER_COMPLETION=$POLL_TRANSFER_COMPLETION"
    -e "POLL_INTERVAL_SECONDS=$POLL_INTERVAL_SECONDS"
    -e "BENCHMARK_RUN_ID=$K6_RUN_ID"
  )
  if [ -n "$PRE_ALLOCATED_VUS" ]; then K6_COMMAND+=( -e "PRE_ALLOCATED_VUS=$PRE_ALLOCATED_VUS" ); fi
  if [ -n "$MAX_VUS" ]; then K6_COMMAND+=( -e "MAX_VUS=$MAX_VUS" ); fi
  K6_COMMAND+=(
    --summary-export="$K6_SUMMARY"
    --out "json=$K6_SAMPLES"
    "$REPO_ROOT/benchmark/k6/ledger.js"
  )
  "${K6_COMMAND[@]}" > "$RESULT_DIR/k6-console.txt" 2>&1 || K6_STATUS=$?
else
  case "$RESULT_DIR" in
    "$REPO_ROOT"/*) ;;
    *) echo "Docker k6 fallback requires RESULT_DIR inside $REPO_ROOT" >&2; exit 1 ;;
  esac
  RESULT_CONTAINER=/work/${RESULT_DIR#"$REPO_ROOT"/}
  DATA_CONTAINER=/work/${BENCHMARK_DATA_FILE#"$REPO_ROOT"/}
  DOCKER_BASE_URL=${DOCKER_BASE_URL:-http://host.docker.internal:3000}
  K6_ENV=(
    -e "BASE_URL=$DOCKER_BASE_URL"
    -e "BENCHMARK_DATA_FILE=$DATA_CONTAINER"
    -e "TARGET_TPS=$TARGET_TPS"
    -e "WARMUP_TPS=$WARMUP_TPS"
    -e "WARMUP_DURATION=$WARMUP_DURATION"
    -e "WARMUP_GRACEFUL_STOP=$WARMUP_GRACEFUL_STOP"
    -e "WARMUP_SETTLE_DURATION=$WARMUP_SETTLE_DURATION"
    -e "DURATION=$DURATION"
    -e "GRACEFUL_STOP=$GRACEFUL_STOP"
    -e "POLL_TRANSFER_COMPLETION=$POLL_TRANSFER_COMPLETION"
    -e "POLL_INTERVAL_SECONDS=$POLL_INTERVAL_SECONDS"
    -e "BENCHMARK_RUN_ID=$K6_RUN_ID"
  )
  if [ -n "$PRE_ALLOCATED_VUS" ]; then K6_ENV+=( -e "PRE_ALLOCATED_VUS=$PRE_ALLOCATED_VUS" ); fi
  if [ -n "$MAX_VUS" ]; then K6_ENV+=( -e "MAX_VUS=$MAX_VUS" ); fi
  docker run --rm -v "$REPO_ROOT:/work" -w /work "${K6_ENV[@]}" "$K6_IMAGE" run \
    --summary-export="$RESULT_CONTAINER/k6-summary.json" \
    --out "json=$RESULT_CONTAINER/k6-samples.json" \
    /work/benchmark/k6/ledger.js > "$RESULT_DIR/k6-console.txt" 2>&1 || K6_STATUS=$?
fi

if [ -n "$PROFILE_PID" ]; then
  wait "$PROFILE_PID" || true
  PROFILE_PID=
fi
curl --silent --show-error --fail --output "$RESULT_DIR/db-stats-final.json" \
  "$PPROF_URL/debug/db-stats" 2>/dev/null || true
"$SCRIPT_DIR/capture-database-state.sh" "$RESULT_DIR/database-state.txt" || true
"$SCRIPT_DIR/capture-query-plans.sh" "$RESULT_DIR/query-plans.txt" || true

if [ -f "$K6_SAMPLES" ]; then
  gzip -9 "$K6_SAMPLES"
  K6_SAMPLES="$K6_SAMPLES.gz"
fi

DURATION_SECONDS=$(python3 - "$DURATION" <<'PY'
import re, sys
parts = re.findall(r'(\d+(?:\.\d+)?)(ms|s|m|h)', sys.argv[1].replace(' ', ''))
scale = {'ms': .001, 's': 1, 'm': 60, 'h': 3600}
print(sum(float(value) * scale[unit] for value, unit in parts))
PY
)
if [ -f "$K6_SAMPLES" ]; then
  "$SCRIPT_DIR/summarize-k6.py" "$K6_SAMPLES" --duration "$DURATION_SECONDS" \
    --output "$RESULT_DIR/per-scenario-summary.json"
fi

echo "results=$RESULT_DIR"
echo "k6_exit_status=$K6_STATUS"
exit "$K6_STATUS"
