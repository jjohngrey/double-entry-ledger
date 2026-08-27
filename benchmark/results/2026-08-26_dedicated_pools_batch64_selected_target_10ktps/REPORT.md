# Stage 11 — fixed connection budgets and larger posting batches

## Verdict

This stage does **not** pass 10,000 logical operations/s or p99 below 50 ms.
The selected profiled run completed 3,018.0 successful operations/s at
2,382.2 ms p99 with zero logical errors. That is effectively identical to the
stage-10 profiled repeat (3,019.8/s at 2,698 ms p99), so it does not
substantiate a throughput gain.

The best unprofiled repeat completed 3,658.1/s at 1,516 ms p99, versus the
prior unprofiled best of 3,637.1/s. The 0.6% difference is smaller than the
observed run-to-run variance and is not presented as a capacity improvement.

## Changes under test

- The existing 90-connection ceiling is partitioned into foreground (70),
  transfer completion (2), event publication (14), and aggregate projection
  (4) pools. Completion waiters remain shared, and diagnostics report both the
  aggregate and per-pool statistics. PostgreSQL still has
  `max_connections=100`; this stage does not hide contention by exceeding it.
- Non-idempotent posting batches increase from 32 to 64 operations. The same
  account validation, deterministic locking, atomic entries/balance/outbox
  write, and single WAL commit are retained.

## Retained sweep

All values are measurement-tagged completions divided by the 10-second arrival
window. Each run had zero logical errors.

| Configuration | Successful/s | Overall p99 | Normal/s | Transfer/s | Aggregate/s |
|---|---:|---:|---:|---:|---:|
| Stage 10, shared pool, profiled | 3,019.8 | 2,698 ms | 765.1 | 331.3 | 275.1 |
| Eight posting writers, shared pool | 2,528.7 | 4,147 ms | 736.7 | 277.6 | 143.8 |
| Pools 70/2/14/4, posting batch 32 | 3,464.2 | 1,649 ms | 691.5 | 489.5 | 515.9 |
| Pools 80/1/5/4, posting batch 32 | 2,983.4 | 2,102 ms | 579.7 | 383.1 | 409.4 |
| **Pools 70/2/14/4, posting batch 64** | **3,658.1** | **1,516 ms** | **917.9** | **483.7** | **583.0** |
| **Same selection, profiled repeat** | **3,018.0** | **2,382 ms** | **729.9** | **382.3** | **435.4** |

The 64-operation batch improved the same-sweep unprofiled result over batch 32
by 5.6% overall and 32.7% for normal posting. Profiling overhead and host
variance erase that gain in the exact repeat, so the honest conclusion is
that the change is directionally useful but not enough to move the target.

## Profile and remaining bottleneck

The final aggregate pool snapshot records 37,482 waits and 4,736.1 cumulative
seconds of wait time. Of that, the foreground pool accounts for 34,655 waits
and 4,727.3 seconds; transfer and aggregate pools record no waits. Pool
partitioning therefore prevents background starvation but does not create
database capacity.

The CPU profile remains syscall/event-wait dominated (`syscall.rawsyscalln`
46.5%, `runtime.kevent` 12.5%). PostgreSQL queries and commit/WAL work—not Go
business logic—remain the limiting resource on this single Docker-hosted
PostgreSQL instance.

Two further experiments were rejected rather than retained in production:
source-transfer ingress batching produced 2,532.0/s with one writer and
2,558.6/s with four; typed-array/`UNNEST` bind compression produced 3,006.8/s.
Neither improved throughput.

The next credible path to 10k is architectural: isolate the aggregate
projection database from OLTP, then scale the HTTP/posting tier horizontally
against a PostgreSQL deployment with measured WAL/fsync capacity. Before that,
a safe PostgreSQL configuration sweep (buffer sizing, checkpoints, WAL sizing,
JIT off) may produce an incremental gain, but disabling synchronous commit
would weaken ledger durability and is not recommended as a benchmark shortcut.

## Correctness

The full PostgreSQL-backed suite, code-only suite, `go vet`, shell syntax, and
whitespace checks pass after the retained change. The pool tests prove an exact
90-session partition and rejection of partial/oversubscribed configurations;
the store test proves dedicated workers share completion notifications.
