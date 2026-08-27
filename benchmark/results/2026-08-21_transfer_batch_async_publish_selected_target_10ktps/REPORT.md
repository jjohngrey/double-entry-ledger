# Stage 10 — transfer completion batching and asynchronous publication

## Verdict

The two requested changes are correct and materially improve transfer fairness
and the sustainable control, but they **do not pass** the 10,000 operations/s /
p99 below 50 ms gate and do not raise the prior headline target yield.

At the selected one-transfer/16-publisher configuration, the best 10k repeat
completed 3,637.1 successful operations/s at 1,696.3 ms p99. The exact profiled
repeat completed 3,019.8/s at 2,698 ms p99. Stage 9 completed 3,818.6/s at
3,162 ms p99. Thus stage 10 cuts tail latency and more than doubles transfer
completion yield, but total yield is 4.8% lower in the best repeat and 20.9%
lower in the profiled repeat. All retained runs have zero logical errors.

The captures were made from the stage-10 working tree before its commit, so
`environment.txt` records the preceding source-snapshot `git_head`. The commit
that contains this report also contains the exact tested source files; future
runs additionally record `git_worktree_dirty` to make this state explicit.

## Changes under test

### Multi-saga destination completion

The transfer worker now claims up to 100 ready destination-credit events in one
transaction, ensures clearing accounts in bulk, locks the union of destination
and clearing accounts in deterministic UUID order, groups balance deltas by
account, and performs bulk transaction, entry, balance, saga, saga-step,
transaction-outbox, and transfer-outbox writes with one commit. Replay remains
a no-op. Rare source compensations drain on one separate worker so idle normal
workers do not issue a second empty transaction.

### Bounded asynchronous JetStream publication

The publisher leases and hydrates a batch, submits its messages with
`PublishAsync`, bounds the JetStream acknowledgement window at 256, waits for
each future, then bulk-marks successful rows and reschedules only failed
acknowledgements. It retains at-least-once semantics: a crash after broker
acceptance but before the outbox update may redeliver, and the durable consumer
inbox absorbs that duplicate.

## Worker sweep

All rates are measurement-tagged completions divided by the 10-second arrival
window, not wall-clock drain throughput.

| Configuration | Successful yield/s | Overall p99 | Transfer yield/s | Transfer p99 | Aggregate yield/s | Aggregate p99 |
|---|---:|---:|---:|---:|---:|---:|
| Stage 9: 16 transfer / 16 synchronous publisher | 3,818.6 | 3,162 ms | 198.5 | 3,381 ms | 517.5 | 2,053 ms |
| 4 transfer / 4 async publisher, profiled | 2,816.7 | 4,400 ms | 598.1 | 1,596 ms | 193.9 | 4,848 ms |
| 4 transfer / 16 async publisher | 3,063.6 | 3,978 ms | 638.5 | 1,447 ms | 152.5 | 5,198 ms |
| **1 transfer / 16 async publisher, best repeat** | **3,637.1** | **1,696 ms** | **481.4** | **1,367 ms** | **354.0** | **1,934 ms** |
| 1 transfer / 8 async publisher | 2,125.5 | 4,249 ms | 215.8 | 3,992 ms | 174.7 | 4,505 ms |
| **1 transfer / 16 async publisher, profiled repeat** | **3,019.8** | **2,698 ms** | **331.3** | **2,773 ms** | **275.1** | **2,933 ms** |

Four transfer workers maximize transfer completions but crowd foreground and
projection work out of the shared 90-connection pool. One worker can drain full
100-saga batches and gives the best overall balance. Async publisher scaling is
also non-linear: four underfeeds the aggregate consumer, while sixteen is the
best tested setting. Run-to-run target variance remains material on the shared
host and is reported rather than hidden.

## Selected 1k control

The selected default sustains the complete offered workload with 1,000.4
successful operations/s, zero drops, zero errors, and zero pool waits.

| Scenario | Successful/s | p50 | p95 | p99 | Error rate |
|---|---:|---:|---:|---:|---:|
| Normal balanced posting | 500.1 | 6.97 ms | 17.48 ms | 48.13 ms | 0% |
| Idempotent duplicate posting | 200.1 | 1.88 ms | 4.48 ms | 16.68 ms | 0% |
| Cross-ledger transfer completion | 150.1 | 21 ms | 44 ms | 91 ms | 0% |
| Aggregate-event ingestion | 150.1 | 22 ms | 48 ms | 90 ms | 0% |
| **Overall** | **1,000.4** | **7.54 ms** | **33 ms** | **63.62 ms** | **0%** |

Compared with stage 8's 1k control, overall p99 improves from 90 ms to 63.62
ms, transfer from 121 ms to 91 ms, aggregate from 145 ms to 90 ms, and normal
posting remains below its individual 50 ms gate. The mixed end-to-end gate
still fails because transfer and aggregate p99 remain above 50 ms.

## Remaining bottleneck

The selected profiled target again saturates the 90-connection pool
(`WaitCount=31,321`, cumulative `WaitDuration=1,293.19s`). The CPU profile is
dominated by syscalls and event waiting, not application compute. Faster
destination completion creates additional durable transaction-posted events;
that work competes with foreground posting and exact projection for the same
PostgreSQL sessions and aggregate rows.

The next architectural step is separate foreground, transfer, publisher, and
projection connection budgets—ideally separate database ownership for the
projection pipeline—plus sharded/micro-batched aggregate rows. Raising the
single pool above 90 is not credible because PostgreSQL records
`max_connections=100`.

## Correctness

The PostgreSQL suite proves eight same-destination sagas complete in one batch
with exact balances, one transaction per destination leg, bulk-completed steps
and outbox rows, and replay no-op. Async publication tests prove a failed
acknowledgement is the only event retried while successful rows are completed
in bulk. The complete PostgreSQL and code-only suites, `go vet`, JavaScript and
shell syntax, and whitespace checks pass. See `correctness.txt`.
