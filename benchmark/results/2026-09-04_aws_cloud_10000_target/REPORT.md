# AWS cloud benchmark report, 2026-09-04

## Verdict

This build did not pass the target. During the 30 second measured interval it completed 239,267 logical operations, or 7,975.567 operations per second, with 692 ms overall p99 latency and no logical errors. The required thresholds were 10,000 operations per second and p99 below 50 ms.

The throughput result is also constrained by the load-generator configuration. k6 reached its maximum of 2,745 virtual users and dropped 60,308 scheduled iterations across the full run. Therefore this run proves that latency fails the target, but it does not establish the final server throughput ceiling.

## Measured target-run results

| Scenario | Successful ops/s | Error rate | p50 ms | p95 ms | p99 ms |
| --- | ---: | ---: | ---: | ---: | ---: |
| Normal balanced posting | 3,536.567 | 0.0% | 81.859 | 312.908 | 466.051 |
| Idempotent duplicate posting | 1,871.300 | 0.0% | 65.285 | 248.361 | 300.140 |
| Cross-ledger transfer | 1,330.133 | 0.0% | 215.000 | 635.000 | 809.000 |
| Aggregate-event ingestion | 1,237.567 | 0.0% | 214.000 | 621.000 | 807.000 |
| Overall | 7,975.567 | 0.0% | 106.754 | 474.000 | 692.000 |

The full k6 summary, canonical per-scenario summary, console output, CPU profile, heap profile, PostgreSQL query plans, service log, database state, and CloudWatch metrics are committed alongside this report. The 57 MB compressed per-request sample stream is retained in the private S3 run archive. `full-samples-archive.txt` records its exact object location and SHA-256 checksum without inflating the Git repository.

## Control run

The preceding control completed 15,003 operations in its 15 second measured interval, or 1,000.2 operations per second, with no logical errors. Overall p50 was 5.34 ms, p95 was 19.0 ms, and p99 was 52.0 ms. This established that the topology and metrics path were functional before the target run, but it narrowly missed the latency target even at one tenth of the requested target load.

## Exact environment

The tested application was git commit `36dceb97e84d5765c2bb3c5f25a072c54ab82d6e`.

| Component | Configuration |
| --- | --- |
| Region | AWS us-west-2 |
| API and NATS host | EC2 c7g.xlarge, 4 Graviton3 vCPUs, 8 GiB class memory, Amazon Linux 2023 |
| Load generator | Separate EC2 c7g.xlarge, k6 0.54.0 container |
| Go | go1.26.7-X:nodwarf5 linux/arm64 |
| NATS | 2.11.17 Alpine container, JetStream enabled |
| OLTP database | RDS PostgreSQL 18.3, db.r7g.2xlarge, 400 GiB gp3, 12,000 provisioned IOPS, 500 MiB/s provisioned throughput |
| Projection database | RDS PostgreSQL 18.3, db.r7g.xlarge, 400 GiB gp3, 12,000 provisioned IOPS, 500 MiB/s provisioned throughput |
| OLTP durability | `synchronous_commit=on`, `wal_compression=zstd` |
| OLTP memory | `shared_buffers=16113496kB`, `effective_cache_size=32226992kB`, `work_mem=4MB`, `wal_buffers=64MB` |
| Projection memory | `shared_buffers=8011520kB`, `effective_cache_size=16023040kB`, `work_mem=4MB` |
| Dataset before load | 5,634 accounts, 101,024 transactions, 202,048 entries |
| Dataset after load | 5,634 accounts, 341,160 transactions, 682,320 entries, 42,605 sagas, 383,765 outbox events |
| OLTP database size after load | 391 MB |
| Projection database size after load | 45 MB |
| Test schedule | 15 second warm-up, 30 second measured interval, 10,000 requested operations per second |

## Application concurrency and batching

The OLTP pool budget was 90 connections: 74 foreground, 2 transfer, and 14 publisher. Aggregate projection used a separate 20 connection pool with 4 workers. Posting used 4 workers with batches of 64 and a 500 microsecond wait. Transfer completion used 1 worker with batches of 100. Publication used 16 workers and asynchronous acknowledgement overlap capped at 256 pending messages. Aggregate projection used batches of 64.

## Bottleneck evidence

The infrastructure was not CPU or storage saturated during the target interval. The API host peaked at 38.07% CPU, the k6 host at 48.53%, OLTP RDS at 33.05%, and projection RDS at 5.89%. OLTP write latency was about 1.35 ms in the hottest minute, write IOPS were about 515 per second, and disk queue depth was 0.66. These values are far below the provisioned storage ceiling.

The CPU profile is dominated by syscall wait and allocation overhead rather than a single compute-heavy application function. The 30 second profile collected 51.61 CPU-seconds, only 172% of four available cores. Heap in-use space was 98.54 MB. The largest retained categories were string builders, pgx connection buffers, PostgreSQL statement preparation, and pgx statement caches. `PgConn.Prepare` retained 14.02 MB and the pgx LRU statement caches retained 8.20 MB.

Query plans for point lookups were sub-millisecond and used the intended indexes. The notable exception was the synthetic account-history query, which returned 100,000 rows and spilled a 7.1 MB sort to disk, completing in 77.95 ms. The hot posting and transfer requests do not use that history query. The query-plan fixture itself used name-based subqueries that scanned 5,634 accounts; production requests use seeded account IDs, so those scans are not evidence of the measured hot-path bottleneck.

The strongest immediate constraint was k6 concurrency. At high latency, 2,745 VUs were insufficient to maintain the configured arrival rate. The next valid capacity run must increase the k6 VU ceiling enough to avoid dropped iterations. That will make the generator apply the full 10,000 operations per second, but it will not by itself fix the p99 target.

Application pool statistics confirm substantial queueing. The captured snapshot showed 23,949 foreground pool waits totaling 273.9 seconds and 490,058 publisher pool waits totaling 58.7 seconds since process start. The transfer pool recorded no waits. The counters are cumulative, so they cannot assign all of that delay to the measured 30 second interval, but they establish that foreground and publisher connection acquisition were active constraints during the benchmark session.

The remaining server-side latency is consistent with queueing behind the 74-connection foreground pool and synchronous multi-statement transactions, especially for transfer and aggregate scenarios. Because CPU, RDS CPU, storage latency, and storage queue depth all had substantial headroom, buying more RDS IOPS alone is unlikely to improve this result materially.

## Next experiment

1. Raise k6 `maxVUs` to at least 8,000 and use a larger load-generator instance if required, then verify that `dropped_iterations` is zero.
2. Run a rate sweep at 4k, 6k, 8k, and 10k operations per second to locate the latency knee instead of testing only the endpoint.
3. Record PostgreSQL wait events and application pool-acquire latency during each step. This will separate database lock time from application pool queue time.
4. Sweep the foreground pool while keeping reserved worker pools fixed. Test 70, 120, and 180 foreground connections and stop increasing when RDS CPU, lock waits, or p99 worsens.
5. Reduce per-request statement preparation and encoding allocation only after the wait data confirms those costs are significant. The profiles show memory overhead, but they do not show a CPU-bound server.

Durability was not weakened for this run. `synchronous_commit` remained enabled.
