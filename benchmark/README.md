# Ledger benchmark methodology

This directory contains the reproducible k6 load suite, a destructive seed for
an isolated database, pprof and PostgreSQL plan capture, and committed raw
evidence. The current result is summarized in [REPORT.md](REPORT.md), and the
ordered optimization lineage is recorded in [RUN_STAGES.md](RUN_STAGES.md).

## Safety and prerequisites

The seeder refuses to reset a database unless its name contains `benchmark`.
Compose exposes dedicated PostgreSQL and NATS instances on ports 5433 and 4223
with separate volumes; normal development data on ports 5432/4222 is untouched.

Docker is required. PostgreSQL 16.13, NATS 2.11.17-alpine, and the fallback k6
0.54.0 image are digest-pinned. Compose waits for both benchmark services to be
healthy; the seeder also retries cold starts. Start a short run with:

```bash
make benchmark-smoke
```

The ticket target is the default for:

```bash
make benchmark
```

Threshold failures intentionally make the target return non-zero, but the
runner still saves its summary, gzip-compressed raw samples, server log,
environment, post-load backlog, database plans, pool stats, and CPU/heap
profiles. When it owns the server process, the runner refuses to start if an
HTTP service is already responding on the benchmark address.

## Workload and seed

`cmd/benchmark-seed` migrates and resets `ledger_benchmark`, clears the isolated
JetStream stream, and writes `benchmark/data.json`. Its default dataset is:

- 512 isolated account sets for each scenario;
- 100,000 historical balanced transactions;
- 5,634 accounts, 101,024 transactions, 202,048 entries, and 101,024 processed
  outbox rows before load;
- pre-completed idempotency fixtures so duplicate timing measures only the
  replay path;
- one source/destination ledger pair per transfer fixture, with valid opening
  balances and pre-created clearing accounts.

The k6 mix treats one iteration as one logical operation and splits the offered
`TARGET_TPS` 50/20/15/15:

1. `normal_balanced_posting`: post a unique balanced two-entry transaction.
2. `idempotent_duplicate_posting`: replay a seeded request/key and require HTTP
   200 plus the original transaction ID.
3. `cross_ledger_transfer`: enqueue a unique transfer and, by default, poll its
   saga to `completed`; enqueue and completion lag are separate metrics.
4. `aggregate_event_ingestion`: post a transaction, then poll that exact
   transaction's projection status until PostgreSQL outbox → JetStream →
   consumer inbox/projection is durable. The status query joins the transaction
   outbox event to `processed_events`; that inbox marker and aggregate updates
   commit atomically. This is attributable end-to-end ingestion lag, not a
   synthetic NATS publish or an unrelated account-cursor change.

Warm-up is a separate mixed scenario capped at 100 ops/s by default. Measurement
starts only after its full graceful-stop allowance plus a settle interval, so
an intentionally overloaded target does not manufacture a warm-up backlog.
Measured submetrics exclude warm-up. Every measured scenario records count,
successful count, arrival-window yield, error rate, and p50/p95/p99. Yield is
measurement-tagged completions divided by the configured arrival window;
graceful-stop completions are included, so it is not mislabeled as wall-clock
drain throughput.

k6 fails the run when a measured scenario drops an iteration, fails to complete
its scheduled count, reaches p99 of 50 ms or more, or reaches 1% logical errors.
A successful run therefore demonstrates both the offered rate and the latency
and correctness gates.

## Reproducibility controls

Key variables and defaults:

| Variable | Default | Meaning |
|---|---:|---|
| `TARGET_TPS` | `10000` | Total offered logical operations/s |
| `WARMUP_TPS` | `min(TARGET_TPS,100)` | Sustainable excluded warm-up rate |
| `WARMUP_DURATION` | `15s` | Excluded mixed warm-up |
| `WARMUP_GRACEFUL_STOP` | `20s` | Finish warm-up operations before measurement |
| `WARMUP_SETTLE_DURATION` | `2s` | Idle interval after warm-up drain |
| `DURATION` | `30s` | Measured interval |
| `GRACEFUL_STOP` | `30s` | Finish admitted measurement iterations |
| `BENCHMARK_ACCOUNT_PAIRS` | `512` | Isolated account sets per scenario |
| `BENCHMARK_HISTORY_TRANSACTIONS` | `100000` | Historical dataset size |
| `DB_MAX_OPEN_CONNS` | `90` | Go pool open cap (leaves PostgreSQL headroom) |
| `DB_MAX_IDLE_CONNS` | `90` | Go pool idle cap |
| `DB_CONN_MAX_LIFETIME` | `30m` | Connection lifetime |
| `DB_CONN_MAX_IDLE_TIME` | `5m` | Idle expiry |
| `WORKER_BATCH_SIZE` | `100` | Outbox drain limit per pass |
| `WORKER_IDLE_DELAY` | `10ms` | Delay only when a worker is idle/errors |
| `TRANSFER_WORKERS` | `1` | Destination-credit batch processor; one worker avoids pool crowding |
| `PUBLISHER_WORKERS` | `16` | Concurrent transaction event publishers |
| `PUBLISH_ASYNC_MAX_PENDING` | `256` | Bounded JetStream publish acknowledgements in flight |
| `AGGREGATE_WORKERS` | `4` | Concurrent durable aggregate consumers |
| `AGGREGATE_FETCH_BATCH` | `64` | JetStream messages requested per aggregate fetch |
| `POSTING_BATCH_SIZE` | `32` | Maximum non-idempotent posts per shared commit |
| `POSTING_BATCH_WAIT` | `500us` | Maximum time used to form a posting micro-batch |
| `POSTING_BATCH_WORKERS` | `4` | Concurrent posting batch writers |
| `BENCHMARK_STAGE` | `unlabeled` | Prospective label written before load starts |
| `BENCHMARK_PREDECESSOR` | `none` | Result directory or stage this run follows |
| `BENCHMARK_CHANGE_UNDER_TEST` | `unspecified` | Exact implementation change being measured |
| `BENCHMARK_OBJECTIVE` | configured workload | Question the run should answer |
| `POLL_INTERVAL_SECONDS` | `0.01` | k6 projection/saga observation interval |
| `PRE_ALLOCATED_VUS` / `MAX_VUS` | target-dependent | Generator concurrency cap |

The runner disables per-request server logging. It resolves/pulls the pinned k6
image before environment capture. `environment.txt` records machine/OS, Go and
k6 versions, exact image references and IDs, Docker CPU/RAM allocation,
PostgreSQL version/settings, pool values, exact dataset cardinality, VU caps,
rates, and every warm-up/measurement timing input. No DSN password is written.
It also records `git_head` and whether the worktree was dirty at capture time.
Before seeding or starting load, `run-metadata.json` records the stage,
predecessor, change under test, objective, and preparation sequence. This keeps
future metrics self-describing and reviewable before commit.

## Profiling and correctness

`PPROF_ADDR` enables a separate diagnostics listener; the runner binds it to
`127.0.0.1:6060`. CPU capture is delayed past warm-up toward the measurement
window; container startup can shift it slightly. The runner then saves a heap
profile, text tops, and `sql.DB.Stats`. `benchmark/sql/query-plans.sql` captures
`EXPLAIN (ANALYZE,
BUFFERS, WAL, SETTINGS)` for entry/history, saga, account lock, outbox, exact
projection status, aggregate UPSERT, batched entry insert, and batched balance
update paths.

Run all correctness, overdraft, transfer, aggregate replay, and idempotency
tests—not only the unit subset—after a performance change:

```bash
DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5433/ledger_benchmark?sslmode=disable' \
  go test ./... -count=1
```

Raw k6 sample files are JSON Lines compressed with gzip. Rebuild the concise
per-scenario JSON with:

```bash
benchmark/scripts/summarize-k6.py path/to/k6-samples.json.gz \
  --duration 30 --output per-scenario-summary.json
```

## Committed evidence

- `2026-08-17_m2pro_baseline`: pre-tuning diagnostic baseline. It predates the
  exact projection observer; successful aggregate timings are not capacity
  evidence.
- `2026-08-17_m2pro_tuned_*`: intermediate profile-led tuning runs retained to
  show the optimization sequence.
- `2026-08-17_m2pro_corrected_100tps`: final exact-observer control run.
- `2026-08-17_m2pro_target_10ktps`: final corrected 10k offered-load attempt,
  including exact environment, raw samples, profiles, plans, backlog, and
  correctness output.
- `2026-08-21_optimized_target_10ktps`: retained optimization pass and final
  profiled 10k attempt. It improves successful arrival-window yield to
  2,458.1/s with zero logical errors, but still fails the 50 ms p99 target; its
  `REPORT.md` documents the comparison and remaining PostgreSQL bottleneck.
- `2026-08-21_source_transfer_batch_1000tps`: zero-drop, zero-error 1k control
  for the retained source-transfer and leased-publisher optimization set.
- `2026-08-21_posting_batch_1000tps`: zero-drop, zero-error 1k control for
  transparent multi-operation posting batches; normal posting is 48.46 ms p99
  and the mixed workload is 90 ms p99.
- `2026-08-21_posting_batch_target_10ktps`: profiled multi-operation batching
  target attempt. Successful arrival-window yield is 3,818.6/s with zero
  logical errors and 3,162 ms overall p99; it honestly fails the 10k / 50 ms
  gate and records the remaining transfer/projection and pool bottlenecks.
- `2026-08-21_transfer_batch_async_publish_*`: stage-10 control, worker sweeps,
  and target repeats for batched multi-saga destination completion and bounded
  asynchronous JetStream publication. The selected 1/16 control sustains
  1,000.4/s at 63.62 ms p99; target repeats improve transfer fairness and tail
  latency but do not exceed stage 9's total yield. The selected profiled
  target's `REPORT.md` documents every retained sweep and the tradeoff.
