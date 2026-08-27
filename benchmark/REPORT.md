# Benchmark report — 2026-08-17

> **Transfer batch / async publication update (2026-08-21):** Stage 10 batches
> destination completion across sagas and overlaps bounded JetStream publish
> acknowledgements. The selected 1/16 control sustains 1,000.4/s with zero
> drops/errors/waits and reduces overall p99 from 90 ms to 63.62 ms. At 10k,
> the best repeat reaches 3,637.1/s at 1.70s p99 and the profiled repeat reaches
> 3,019.8/s at 2.70s p99—better transfer fairness/tails, but below stage 9's
> 3,818.6/s headline yield. See
> [`results/2026-08-21_transfer_batch_async_publish_selected_target_10ktps/REPORT.md`](results/2026-08-21_transfer_batch_async_publish_selected_target_10ktps/REPORT.md).

> **Multi-operation batching update (2026-08-21):** Transparent posting
> micro-batches raise successful arrival-window yield at 10k offered load from
> 2,458.1/s to 3,818.6/s, keep logical errors at 0%, and reduce overall p99
> from 9.06s to 3.16s. The target still fails because 61,278 iterations were
> dropped and p99 remains above 50ms. At 1k offered load, normal posting now
> passes its individual gate at 48.46ms p99; the mixed workload is 90ms p99.
> See
> [`results/2026-08-21_posting_batch_target_10ktps/REPORT.md`](results/2026-08-21_posting_batch_target_10ktps/REPORT.md)
> for the current evidence and remaining bottleneck.

> **Optimization update (2026-08-21):** The publisher, aggregate consumer,
> idempotency path, posting writes, source transfer, completion observation,
> and pool were optimized and re-profiled. The new 10k attempt raises
> successful arrival-window yield from 1,313.6/s to 2,458.1/s, eliminates
> logical errors, and reduces p99 from 15.14s to 9.06s, but still does not pass
> 10k / 50ms. See
> [`results/2026-08-21_optimized_target_10ktps/REPORT.md`](results/2026-08-21_optimized_target_10ktps/REPORT.md)
> for the retained changes, raw evidence, and current bottleneck. The report
> below remains the pre-optimization record.

## Result

The service does **not** substantiate 10,000 logical operations/s or p99 below
50 ms on this environment. At 10,000 offered ops/s, k6 emitted completion
records for 14,143 measurement iterations associated with the 10-second
arrival window: 1,414.3 iterations per scheduled second, of which 1,313.6/s
eventually met their end-to-end success condition. Overall p99 was 15,144.6 ms
and logical error rate was 7.12%.

That 1,414.3/s value is arrival-window yield, not wall-clock drain throughput:
some operations completed during graceful stop. Across the complete 47.7-second
run, including warm-up, the deliberate drain interval, measurement, and
graceful stop, k6 reported 296.4 measurement-tagged completions/s and 275.3
successful completions/s. Its run-wide mean HTTP request rate was 1,185.6/s.
None of these numbers is a 10k capacity claim.

“Operation” means one scenario iteration. It is not an HTTP-request count: the
aggregate and transfer scenarios poll asynchronous completion and amplify
requests. The harness now fails on any measured dropped iteration, incomplete
scheduled count, p99 at or above 50 ms, or logical error rate at or above 1%.

## Exact target environment

- Host: Apple M2 Pro (10 cores), 16 GiB RAM, macOS 26.5.2 / Darwin 25.5.0,
  arm64.
- Docker Desktop: 10 CPUs, 8,214,851,584 bytes RAM; Docker 29.7.2, Compose
  5.4.0.
- Go 1.26.2; k6 0.54.0 pinned to
  `sha256:1f40432b1cbe7234e977f96c362c9bc550a2d2b583d014dd8669fe40d3e9e755`.
- PostgreSQL 16.13 pinned to
  `sha256:71e27bf60b70bded003791b5573f8b808365613f341df20ffcf0c1ed7bc13ddf`;
  NATS 2.11.17-alpine pinned to
  `sha256:e4bf19f15fd3218814a4e3c9e0064e1334bd8aa20d5984b9f1a0afd084f8cc00`.
- PostgreSQL: `max_connections=100`, `shared_buffers=128 MiB`, `work_mem=4 MiB`,
  `effective_cache_size=4 GiB`, `synchronous_commit=on`, `wal_level=replica`,
  `max_wal_size=1 GiB`, `checkpoint_timeout=300s`, `jit=on`; all captured
  sources are in the environment artifact.
- Go pool: 64 max open, 64 max idle, 30-minute lifetime, 5-minute idle expiry.
- Dataset before load: 5,634 accounts, 101,024 transactions, 202,048 entries,
  101,024 outbox rows, approximately 98.9 MB; 512 account sets per scenario.
- Warm-up: 100 ops/s for 5 seconds, then a 20-second graceful drain and 2-second
  settle. It completed 501 iterations with zero drops or interruptions before
  measurement began.
- Measurement: 10 seconds at a 50/20/15/15 posting/duplicate/transfer/aggregate
  mix; 256 preallocated and 512 maximum VUs per scenario (2,048 measured
  maximum); 20-second graceful stop; request logging off. Generator and service
  shared the host/Docker VM.

The complete corrected target capture is
[`results/2026-08-17_m2pro_target_10ktps`](results/2026-08-17_m2pro_target_10ktps).

## Target metrics

| Scenario | Offered/s | Window yield/s | Successful/s | p50 ms | p95 ms | p99 ms | Error rate |
|---|---:|---:|---:|---:|---:|---:|---:|
| Normal balanced posting | 5,000 | 655.8 | 655.8 | 553.5 | 2,208.5 | 3,382.9 | 0% |
| Idempotent duplicate | 2,000 | 655.1 | 655.1 | 529.8 | 2,281.9 | 3,360.5 | 0% |
| Cross-ledger completion | 1,500 | 52.2 | 2.7 | 15,050.0 | 15,223.8 | 15,372.4 | 94.8% |
| Aggregate ingestion | 1,500 | 51.2 | 0.0 | 15,038.0 | 15,197.0 | 15,344.8 | 100% |
| **Overall** | **10,000** | **1,414.3** | **1,313.6** | **607.8** | **15,017.9** | **15,144.6** | **7.12%** |

All 56,566 completed HTTP requests had an expected status. The failure is
capacity and convergence, not response validation: measured scenarios dropped
85,848 scheduled iterations (43,436 normal, 13,446 duplicate, 14,476 transfer,
14,490 aggregate). At shutdown, 5,514 committed `transaction_posted` events
remained pending. The final pool snapshot recorded 50,607 waits and 23,411.6
aggregate seconds of wait time.

## Corrected 100 ops/s control

The final code and exact transaction-to-projection observer sustain the offered
100 ops/s with no drops, no logical errors, no pool waits, and 100.2 successful
iterations per scheduled second. The overall p99 is still 67.09 ms, so the
mixed workload does not meet the 50 ms objective even at this load.

| Scenario | Successful/s | p50 ms | p95 ms | p99 ms | Error rate |
|---|---:|---:|---:|---:|---:|
| Normal balanced posting | 50.1 | 3.49 | 7.41 | 19.27 | 0% |
| Idempotent duplicate | 20.0 | 2.33 | 4.23 | 11.10 | 0% |
| Cross-ledger completion | 15.1 | 22.0 | 40.5 | 87.5 | 0% |
| Aggregate ingestion | 15.0 | 19.0 | 42.6 | 90.34 | 0% |
| **Overall** | **100.2** | **3.80** | **28.0** | **67.09** | **0%** |

Raw control evidence is in
[`results/2026-08-17_m2pro_corrected_100tps`](results/2026-08-17_m2pro_corrected_100tps).

The pre-tuning baseline admitted 88.37 iterations per scheduled second at 100
offered, with 15,051 ms p99, 10.10% errors, and 6,989 pool waits. That capture
predates exact projection correlation, so its successful aggregate timings are
not used as capacity evidence; its timeouts, profiles, and foreground-path
measurements remain useful diagnostic baseline data. Intermediate tuning
captures are committed for traceability.

## Profile-led changes

The baseline CPU profile, pool waits, and plans showed database/socket round
trips as the first constraint. A representative duplicate/event entry lookup
was a parallel sequential scan over 202,048 entries: 22.714 ms and 2,398
buffers. The tuned plan uses `entries_transaction_id_idx`: 0.028 ms and five
buffers. Saga ID/key lookups use their PK/unique indexes (0.024/0.021 ms), and
the partial transaction outbox queue plan completes in 0.020 ms.

Based on that evidence, the implementation now:

- locks all posting accounts once in deterministic UUID order;
- batches every transaction's entry insert and per-account balance updates;
- uses explicit row locks under `READ COMMITTED`, removing false SSI conflicts
  while preserving overdraft and idempotency guarantees;
- adds transaction-entry, account-history, and partial ready-outbox indexes;
- separates background inbox/outbox isolation from foreground transactions;
- continuously drains work up to the configured batch instead of sleeping
  250 ms between every 100 events;
- splits UUID versus idempotency-key saga lookup so indexes are usable;
- bounds and records the Go connection pool and exposes localhost-only pprof
  plus pool diagnostics;
- correlates aggregate completion to the exact posted transaction through an
  indexed outbox/inbox status query, rather than accepting an unrelated account
  cursor change.

On the corrected control dataset, the exact projection-status plan uses
`outbox_transaction_event_unique` plus the `processed_events` primary key in
0.025 ms. Representative daily account/ledger UPSERT plans complete in
0.116/0.061 ms. Their individual plans are fast; concurrent hot-row contention
and per-event round trips are not visible in a single `EXPLAIN`.

The full PostgreSQL-backed suite passed after batching, after index/isolation
changes, after row-locking, and after exact projection correlation:

```text
go test ./... -count=1
ok github.com/jjohngrey/double-entry-ledger/internal/http
ok github.com/jjohngrey/double-entry-ledger/internal/ledger
```

Coverage includes concurrent overdraft prevention, same-key atomicity,
failed-key reuse, more-than-two/repeated-account batching, transfer replay and
boundary checks, aggregate replay, consumer restart idempotency, and the exact
pending → published → projected status transition.

## Remaining bottleneck

At target load the process is database/socket-I/O bound, not Go compute bound.
The measurement-focused CPU profile spends 57.7% of samples in raw syscalls and
15.1% in `kevent`; all 64 DB connections saturate during load. Each outbox
event is still selected, hydrated, published, marked, and projected through
separate database/broker round trips. The publisher, transfer drain, and
aggregate consumer remain single logical workers. Client polling adds HTTP and
database demand precisely when those workers fall behind.

The next credible step is a leased multi-row outbox claim with batched entry
hydration, concurrent/sharded publishers and aggregate consumers, and
micro-batched projection deltas. A notification or cursor-wait API would remove
polling amplification. Re-test the pool only after reducing round trips;
raising it toward PostgreSQL's 100-connection ceiling would move the current
queue into the database. A separate load-generator host and
`pg_stat_statements` capture are also required before making a production
capacity claim.
