#!/usr/bin/env python3
import argparse
import gzip
import json
import math
from collections import defaultdict
from pathlib import Path


def percentile(values, quantile):
    if not values:
        return None
    ordered = sorted(values)
    position = (len(ordered) - 1) * quantile
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def rounded(value):
    return None if value is None else round(value, 3)


parser = argparse.ArgumentParser(description="Summarize tagged ledger metrics from k6 JSON output")
parser.add_argument("input", type=Path)
parser.add_argument("--duration", type=float, required=True, help="measured duration in seconds")
parser.add_argument("--output", type=Path)
args = parser.parse_args()

open_input = gzip.open if args.input.suffix == ".gz" else open
latencies = defaultdict(list)
errors = defaultdict(list)
operations = defaultdict(float)
successes = defaultdict(float)

with open_input(args.input, "rt", encoding="utf-8") as stream:
    for line in stream:
        point = json.loads(line)
        if point.get("type") != "Point":
            continue
        data = point.get("data", {})
        tags = data.get("tags", {}) or {}
        if tags.get("phase") != "measure":
            continue
        operation = tags.get("operation")
        if not operation:
            continue
        metric = point.get("metric")
        value = float(data.get("value", 0))
        if metric == "ledger_operation_latency_ms":
            latencies[operation].append(value)
        elif metric == "ledger_error_rate":
            errors[operation].append(value)
        elif metric == "ledger_logical_operations":
            operations[operation] += value
        elif metric == "ledger_successful_logical_operations":
            successes[operation] += value

result = {
    "measurement_duration_seconds": args.duration,
    "throughput_semantics": (
        "measurement-tagged completions divided by the configured arrival window; "
        "completions during graceful stop are included, so this is admission-window "
        "yield rather than wall-clock drain rate"
    ),
    "operations": {},
}
all_latencies = []
total_errors = []
for operation in sorted(set(latencies) | set(operations) | set(errors)):
    values = latencies[operation]
    error_values = errors[operation]
    count = operations[operation]
    all_latencies.extend(values)
    total_errors.extend(error_values)
    result["operations"][operation] = {
        "count": int(count),
        "successful_count": int(successes[operation]),
        "throughput_ops_per_second": rounded(count / args.duration),
        "successful_throughput_ops_per_second": rounded(successes[operation] / args.duration),
        "error_rate": rounded(sum(error_values) / len(error_values)) if error_values else None,
        "latency_ms": {
            "p50": rounded(percentile(values, 0.50)),
            "p95": rounded(percentile(values, 0.95)),
            "p99": rounded(percentile(values, 0.99)),
        },
    }

total_count = sum(operations.values())
result["overall"] = {
    "count": int(total_count),
    "successful_count": int(sum(successes.values())),
    "throughput_ops_per_second": rounded(total_count / args.duration),
    "successful_throughput_ops_per_second": rounded(sum(successes.values()) / args.duration),
    "error_rate": rounded(sum(total_errors) / len(total_errors)) if total_errors else None,
    "latency_ms": {
        "p50": rounded(percentile(all_latencies, 0.50)),
        "p95": rounded(percentile(all_latencies, 0.95)),
        "p99": rounded(percentile(all_latencies, 0.99)),
    },
}

rendered = json.dumps(result, indent=2, sort_keys=True) + "\n"
if args.output:
    args.output.write_text(rendered, encoding="utf-8")
else:
    print(rendered, end="")
