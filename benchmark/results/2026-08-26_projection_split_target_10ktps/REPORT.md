# Stage 12 — separate projection PostgreSQL storage

## Verdict

The split is correct but does **not** improve this benchmark environment. The
10k run completed 1,440.3 successful operations/s at 4,446 ms p99 with zero
logical errors, down from the prior 3,018.0/s profiled baseline and the
3,658.1/s best unprofiled repeat.

The follow-up with a four-session projection pool completed 850.0/s at
8,102 ms p99. Therefore, connection count is not the cause of the regression.

## Design under test

`PROJECTION_DATABASE_URL` moves `processed_events`,
`daily_account_aggregates`, and `daily_ledger_aggregates` to an independent
PostgreSQL database. The JetStream aggregate consumer commits its inbox marker
and aggregate changes atomically there. OLTP retains accounts, transactions,
entries, balances, sagas, and outbox events.

Projection status remains exact: the handler finds the immutable
`transaction_posted` event in OLTP, then checks that event's consumer inbox
marker in the projection database. It cannot report projected before the
projection transaction commits.

## Evidence

The split-mode 100/s smoke completed 100.6/s with zero errors and 28.28 ms
overall p99. Final state confirms OLTP projection tables remain empty while
the projection database contains the durable inbox and aggregate rows.

At 10k, both PostgreSQL containers ran inside the same Docker Desktop VM, on
the same CPU, memory allocation, and storage path. The second database added
WAL/fsync and scheduling competition rather than isolating it. The result is
evidence that this architecture needs a physically separate projection
deployment (or at least an independent storage/I/O budget) to improve
capacity; two databases on the same machine are not a valid capacity proxy.

## Correctness

The split-mode smoke test, complete PostgreSQL-backed suite, code-only suite,
`go vet`, and shell syntax checks pass. The default one-database behavior
remains unchanged when `PROJECTION_DATABASE_URL` is unset.
