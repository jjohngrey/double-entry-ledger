#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 OUTPUT" >&2
  exit 2
fi

OUTPUT=$1
python3 - "$OUTPUT" <<'PY'
import json
import os
import sys
from datetime import datetime, timezone

metadata = {
    "schema_version": 1,
    "captured_before_load": True,
    "captured_utc": datetime.now(timezone.utc).isoformat(),
    "stage": os.environ.get("BENCHMARK_STAGE", "unlabeled"),
    "predecessor": os.environ.get("BENCHMARK_PREDECESSOR", "none"),
    "change_under_test": os.environ.get("BENCHMARK_CHANGE_UNDER_TEST", "unspecified"),
    "objective": os.environ.get("BENCHMARK_OBJECTIVE", "measure the configured workload"),
    "pre_run_steps": [
        "start the digest-pinned isolated PostgreSQL and NATS services and wait for health",
        "refuse to run if another HTTP service is already listening on the benchmark address",
        "migrate and destructively reseed only the dedicated ledger_benchmark database",
        "reset the dedicated JetStream stream",
        "build the ledger binary from the current working tree",
        "capture the exact environment, dataset, pool, worker, rate, duration, and VU configuration",
        "run the excluded warm-up, allow it to drain, then wait the configured settle period",
        "capture measurement metrics and optional CPU/heap profiles",
        "capture final pool statistics, database state, query plans, raw k6 samples, and summaries",
    ],
}
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    json.dump(metadata, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY
