# Multi-operation posting batch benchmark — 2026-08-21

## Verdict

The implementation still **does not pass** the full 10,000 logical operations/s
and p99 below 50 ms gate. Transparent multi-operation posting batches improve
the correct arrival-window yield from 2,458.1/s to 3,818.6/s (+55.3%), retain a
0% logical error rate, and reduce overall p99 from 9,056 ms to 3,162 ms.

The run completed 38,186 measurement-tagged operations associated with the
10-second arrival window. It dropped 61,278 scheduled iterations, so 3,818.6/s
is arrival-window yield rather than a claim of 10k capacity or wall-clock drain
throughput.

## Change under test

Non-idempotent `POST /transactions` calls are transparently micro-batched. A
batch locks the union of account rows once in deterministic UUID order,
evaluates operations in input order against cumulative balances, then uses one
PostgreSQL command and one commit for all accepted transaction, entry, balance,
and outbox writes. An invalid or overdrawn operation is rejected individually
without poisoning valid neighbors. Idempotent calls retain their existing
atomic key path.

This run used a maximum batch size of 32, a 500 microsecond formation window,
and four concurrent batch writers. The application still uses one
`database/sql` pool capped at 90 open and 90 idle connections; that is one pool
with up to 90 PostgreSQL sessions, not 90 separate pools.

## Target results

| Scenario | Offered/s | Successful yield/s | p50 ms | p95 ms | p99 ms | Error rate |
|---|---:|---:|---:|---:|---:|---:|
| Normal balanced posting | 5,000 | 1,282.9 | 328.11 | 618.61 | 783.22 | 0% |
| Idempotent duplicate posting | 2,000 | 1,819.7 | 18.43 | 105.28 | 181.27 | 0% |
| Cross-ledger completion | 1,500 | 198.5 | 2,711 | 3,323 | 3,381 | 0% |
| Aggregate-event ingestion | 1,500 | 517.5 | 883 | 1,857 | 2,053.26 | 0% |
| **Overall** | **10,000** | **3,818.6** | **179.67** | **1,943.75** | **3,162** | **0%** |

| Comparable run | Successful yield/s | Overall p99 | Drops | Error rate |
|---|---:|---:|---:|---:|
| Pre-optimization target | 1,313.6 | 15,144.58 ms | 85,848 | 7.12% |
| Optimized, before multi-operation batching | 2,458.1 | 9,056 ms | 75,410 | 0% |
| **Multi-operation batching** | **3,818.6** | **3,162 ms** | **61,278** | **0%** |

The final pool snapshot recorded `WaitCount=15,244` and cumulative
`WaitDuration=286.35s`. The CPU profile is dominated by syscalls and event
waiting rather than application compute. Cross-ledger completion is now the
largest p99 contributor, followed by exact aggregate projection.

## Sustainable control

At 1,000 offered operations/s the same implementation completed 10,002
measurement operations, with zero drops, zero errors, and zero pool waits.
Normal posting now passes its individual 50 ms gate at 48.46 ms p99. The mixed
end-to-end workload remains above the gate because transfer completion is
121 ms p99 and aggregate ingestion is 145 ms p99; overall p99 is 90 ms.

Raw control evidence is in `../2026-08-21_posting_batch_1000tps`.

## Remaining bottleneck

Batching materially reduces foreground posting round trips, but the full
workload still shares one durable PostgreSQL instance and one 90-connection
pool among HTTP posting, transfer workers, outbox publishers, aggregate
consumers, and completion observation. Raising only the pool cap would exceed
the recorded PostgreSQL `max_connections=100` headroom and would move the queue
into PostgreSQL rather than remove the work.

The next credible throughput step is to batch multiple transfer sagas and
their destination credits, then separate foreground and background connection
budgets. Reaching 10k durable end-to-end operations on this machine likely
requires partitioned write ownership or separate PostgreSQL instances for
projection/background work; disabling synchronous durability was not used.

## Correctness

The full PostgreSQL-backed suite, code-only suite, `go vet`, k6 JavaScript
syntax, benchmark shell syntax, and whitespace checks pass. Batch regressions
cover multiple committed transactions, cumulative overdraft prevention,
partial per-operation rejection, and delivery of every concurrent caller's
response. See `correctness.txt`.
