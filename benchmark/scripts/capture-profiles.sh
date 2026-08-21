#!/usr/bin/env bash
set -euo pipefail

RESULT_DIR=${1:?usage: capture-profiles.sh RESULT_DIR BINARY}
BINARY=${2:?usage: capture-profiles.sh RESULT_DIR BINARY}
PPROF_URL=${PPROF_URL:-http://127.0.0.1:6060}
PROFILE_SECONDS=${PROFILE_SECONDS:-30}
PROFILE_DELAY_SECONDS=${PROFILE_DELAY_SECONDS:-0}
GOCACHE=${GOCACHE:-/tmp/double-entry-ledger-go-cache}

mkdir -p "$RESULT_DIR"
if [ "$PROFILE_DELAY_SECONDS" != "0" ]; then
  sleep "$PROFILE_DELAY_SECONDS"
fi
curl --silent --show-error --fail --max-time "$((PROFILE_SECONDS + 15))" \
  --output "$RESULT_DIR/cpu.pprof" \
  "$PPROF_URL/debug/pprof/profile?seconds=$PROFILE_SECONDS"
curl --silent --show-error --fail --output "$RESULT_DIR/heap.pprof" \
  "$PPROF_URL/debug/pprof/heap"
curl --silent --show-error --fail --output "$RESULT_DIR/db-stats.json" \
  "$PPROF_URL/debug/db-stats"

if go tool pprof -h >/dev/null 2>&1; then
  PPROF=(go tool pprof)
else
  PPROF_BINARY=${TMPDIR:-/tmp}/double-entry-ledger-pprof
  GOCACHE="$GOCACHE" go build -o "$PPROF_BINARY" cmd/pprof
  PPROF=("$PPROF_BINARY")
fi
"${PPROF[@]}" -top -nodecount=40 "$BINARY" "$RESULT_DIR/cpu.pprof" > "$RESULT_DIR/cpu-top.txt"
"${PPROF[@]}" -top -nodecount=40 -sample_index=inuse_space "$BINARY" "$RESULT_DIR/heap.pprof" > "$RESULT_DIR/heap-top.txt"
