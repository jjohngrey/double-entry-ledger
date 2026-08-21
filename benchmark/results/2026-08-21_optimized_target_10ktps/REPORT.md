# Optimized benchmark report — 2026-08-21

## Verdict

The single-node implementation **does not yet pass** the 10,000 TPS / p99 < 50 ms target. It does establish a materially higher, correct ceiling and identifies PostgreSQL transaction/connection capacity as the remaining bottleneck.

The comparable 10-second 10k offered-load run admitted 24,581 operations during the arrival window (2,458.1 operations/s arrival-window yield). Every admitted operation eventually succeeded, but 75,410 iterations were dropped and overall p99 was 9,056 ms.

## Improvements made

- completed idempotency keys use one read-only joined query instead of updating the hot key row;
- normal postings combine transaction, entry, balance, idempotency, and outbox writes into one PostgreSQL command after deterministic account locking;
- source-transfer transaction, entries, balances, saga, steps, and both outbox records commit in one PostgreSQL command;
- transaction events are claimed in recoverable leases, hydrated in one query, published without holding database connections, and completed in bulk;
- aggregate events use short, idempotent micro-batches with grouped account/ledger deltas;
- projection and transfer completion use durable-check long polling instead of repeated 5 ms database polling;
- measured worker mix is 16 transfer workers, 16 publisher workers, and 4 aggregate workers;
- the pool is capped at 90 connections, leaving headroom under PostgreSQL's recorded `max_connections=100`.

## Results

| Run | Offered rate | Arrival-window yield | Successful yield | Error rate | Overall p99 | Drops |
|---|---:|---:|---:|---:|---:|---:|
| Prior target (`2026-08-17_m2pro_target_10ktps`) | 10,000/s | 1,414.3/s | 1,313.6/s | 7.12% | 15,144.58 ms | 85,848 |
| Optimized target (this directory) | 10,000/s | 2,458.1/s | 2,458.1/s | 0.00% | 9,056 ms | 75,410 |
| Optimized sustainable control (`2026-08-21_source_transfer_batch_1000tps`) | 1,000/s | 1,000.2/s | 1,000.2/s | 0.00% | 152.99 ms | 0 |

The optimized 10k run produced 87.1% more successful arrival-window yield than the prior target, removed logical errors, and reduced p99 by 40.2%. These rates count measurement-tagged operations divided by the configured 10-second arrival window; they are not wall-clock drain throughput.

### Optimized 10k scenario results

| Scenario | Successful operations | Successful yield | p50 | p95 | p99 | Error rate |
|---|---:|---:|---:|---:|---:|---:|
| Normal balanced posting | 10,872 | 1,087.2/s | 317.25 ms | 1,236.90 ms | 1,866.24 ms | 0.00% |
| Idempotent duplicate posting | 11,774 | 1,177.4/s | 279.74 ms | 1,184.56 ms | 1,794.60 ms | 0.00% |
| Cross-ledger transfer | 789 | 78.9/s | 7,061 ms | 10,652.40 ms | 10,979.64 ms | 0.00% |
| Aggregate-event ingestion | 1,146 | 114.6/s | 4,896 ms | 7,370.25 ms | 8,306.50 ms | 0.00% |

### 1k control scenario p99

- normal posting: 69.09 ms;
- idempotent duplicate: 33.06 ms (passes the 50 ms gate);
- cross-ledger transfer completion: 177.12 ms;
- exact aggregate projection: 210.00 ms.

## Remaining bottleneck

The optimized 10k run saturated the 90-connection application pool: `WaitCount=29,870` and cumulative `WaitDuration=11,858.8s`. The Go CPU profile contains 8.72 CPU-seconds over a 10.11-second profile on a 10-core host and is dominated by socket/syscall waits, so application CPU is not the limiting resource. PostgreSQL retained synchronous commit and the recorded default/durable settings.

Reaching 10k durable end-to-end operations on this machine requires a larger architectural change, not another index tweak: partition/shard write ownership across PostgreSQL instances, isolate projection/transfer workloads from foreground posting pools, and batch transfer completion across sagas. Disabling synchronous durability would make the comparison invalid for an accounting ledger and was not used.

## Correctness

After the retained optimizations, `DATABASE_URL=... go test ./... -count=1`, the code-only suite, `go vet ./...`, JavaScript syntax validation, and shell syntax validation all passed. New PostgreSQL regressions prove duplicate events inside an aggregate batch apply once and concurrent publisher workers claim each event once. See `correctness.txt` for the recorded commands.
