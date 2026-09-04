#!/usr/bin/env python3
"""Calculate database/sql pool counter deltas for a measured interval."""

import argparse
import json
from pathlib import Path


COUNTERS = (
    "WaitCount",
    "WaitDuration",
    "MaxIdleClosed",
    "MaxIdleTimeClosed",
    "MaxLifetimeClosed",
)


def delta(before, after):
    result = {}
    for key in COUNTERS:
        start = before.get(key, 0)
        end = after.get(key, 0)
        result[key] = end - start
    result["WaitDurationSeconds"] = round(result["WaitDuration"] / 1_000_000_000, 6)
    result["OpenConnectionsAfter"] = after.get("OpenConnections", 0)
    result["InUseAfter"] = after.get("InUse", 0)
    result["IdleAfter"] = after.get("Idle", 0)
    result["MaxOpenConnections"] = after.get("MaxOpenConnections", 0)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("before", type=Path)
    parser.add_argument("after", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    before = json.loads(args.before.read_text())
    after = json.loads(args.after.read_text())
    summary = {"total": delta(before, after), "pools": {}}
    pool_names = sorted(set(before.get("Pools", {})) | set(after.get("Pools", {})))
    for name in pool_names:
        summary["pools"][name] = delta(
            before.get("Pools", {}).get(name, {}),
            after.get("Pools", {}).get(name, {}),
        )
    args.output.write_text(json.dumps(summary, indent=2) + "\n")


if __name__ == "__main__":
    main()
