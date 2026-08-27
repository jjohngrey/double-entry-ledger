# Benchmark run stages

This ledger makes the optimization sequence explicit. Older stage labels are
reconstructed from their committed configurations and reports; new runs write
`run-metadata.json` **before load begins**, so their intent and predecessor are
recorded prospectively rather than inferred afterward.

Every run follows this preparation sequence unless its row notes a difference:

1. Start the digest-pinned isolated PostgreSQL and NATS services and wait for
   both health checks.
2. Verify that no stale service is listening on the benchmark HTTP address.
3. Apply migrations, reset only `ledger_benchmark`, reset its JetStream stream,
   and seed the recorded account/history fixture.
4. Build the ledger binary from the current working tree.
5. Record machine, versions, PostgreSQL settings, pool/worker configuration,
   dataset cardinality, rates, VU limits, durations, and warm-up settings.
6. Run the excluded warm-up, allow it to drain, and wait the settle period.
7. Run k6, capture raw samples and summaries, and—where enabled—capture
   measurement CPU/heap profiles.
8. Capture final pool statistics, database state, backlog, and query plans.

## Retained lineage

| Stage | Result directory | Predecessor | Change or purpose tested before the run |
|---:|---|---|---|
| 0 | `2026-08-17_m2pro_baseline` | none | Initial diagnostic baseline before index, transaction, pool, and worker tuning. |
| 1 | `2026-08-17_m2pro_tuned_100tps` | stage 0 | Batch per-transaction entries/balance writes, add hot-path indexes, configure the pool, and continuously drain workers. |
| 2 | `2026-08-17_m2pro_tuned_100tps_5ms_poll` | stage 1 | Reduce k6 asynchronous completion polling to test whether observation delay dominated the control result. |
| 3 | `2026-08-17_m2pro_tuned_read_committed_100tps` | stage 2 | Use deterministic account row locks with `READ COMMITTED` for foreground posting while retaining correctness tests. |
| 4 | `2026-08-17_m2pro_corrected_100tps` | stage 3 | Replace aggregate cursor inference with exact transaction-to-projection correlation and use clean warm-up fixtures/drain. |
| 5 | `2026-08-17_m2pro_target_10ktps` | stage 4 | Run the corrected harness at the 10k ticket target with profiles, plans, correctness evidence, and backlog capture. |
| 6 | `2026-08-21_source_transfer_batch_1000tps` | stage 5 | Test the read-only duplicate path, single-command normal/source-transfer writes, leased publisher, aggregate micro-batches, long-poll completion, and tuned workers/pool at 1k. |
| 7 | `2026-08-21_optimized_target_10ktps` | stage 6 | Run the retained optimized implementation at the full 10k target and profile the remaining bottleneck. |
| 8 | `2026-08-21_posting_batch_1000tps` | stage 7 | Add transparent multi-operation posting micro-batches (32 maximum, 500 microseconds, four writers) and validate at 1k. |
| 9 | `2026-08-21_posting_batch_target_10ktps` | stage 8 | Run the same posting-batch implementation at 10k offered load with CPU/heap profiles and final correctness evidence. |
| 10 | `2026-08-21_transfer_batch_async_publish_4x4_1000tps` | stage 9 | Validate multi-saga destination completion and bounded async publication with four transfer/four publisher workers at 1k. |
| 11 | `2026-08-21_transfer_batch_async_publish_target_10ktps` | stage 10 | Profile the four/four worker mix at 10k; transfer improved while aggregate delivery became the limiting tradeoff. |
| 12 | `2026-08-21_transfer_batch_async_publish_target_4x16_10ktps` | stage 11 | Restore sixteen async publishers while retaining four transfer batch workers. |
| 13 | `2026-08-21_transfer_batch_async_publish_target_1x16_10ktps` | stage 12 | Reduce transfer processing to one batch worker to protect foreground/projection pool capacity; best stage-10 target repeat. |
| 14 | `2026-08-21_transfer_batch_async_publish_target_1x8_10ktps` | stage 13 | Test the publisher midpoint; eight publishers underfed projection and reduced total yield. |
| 15 | `2026-08-21_transfer_batch_async_publish_1000tps` | stage 13 | Validate the selected one-transfer/16-publisher default at sustainable 1k load. |
| 16 | `2026-08-21_transfer_batch_async_publish_selected_target_10ktps` | stage 15 | Profile the exact selected default at 10k and record correctness plus the remaining shared-pool bottleneck. |

The table lists every committed result directory. Failed development attempts
are excluded; valid worker sweeps remain committed so the selected defaults and
negative findings are reproducible.

## Labeling a future run

```bash
BENCHMARK_STAGE='stage-10-transfer-destination-batch' \
BENCHMARK_PREDECESSOR='2026-08-21_posting_batch_target_10ktps' \
BENCHMARK_CHANGE_UNDER_TEST='batch destination-credit saga operations' \
BENCHMARK_OBJECTIVE='raise transfer throughput without weakening durability' \
make benchmark
```

The runner saves those values and the preparation sequence in
`run-metadata.json` beside the raw results. Result directories are intentionally
tracked; `benchmark/data.json` and the `current-results` scratch pointer remain
ignored.
