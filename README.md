# Double Entry Ledger

A double-entry accounting ledger built in Go + PostgreSQL.

Built alongside reading *Designing Data-Intensive Applications*.

## Setup

### Installation

1. Clone the repository:
```bash
git clone https://github.com/jjohngrey/double-entry-ledger.git
cd double-entry-ledger
```

2. Install dependencies:
```bash
go mod download
```

3. Build and run:
```bash
make run
```

The server will start on `http://localhost:3000`.

`docker compose up -d` starts PostgreSQL and NATS with JetStream. The service
uses `NATS_URL` (default `nats://localhost:4222`). Every committed posting is
published as `transaction_posted`; projections are intentionally eventually
consistent. Their freshness cursor is returned by:

- `GET /accounts/{id}/aggregates`
- `GET /ledgers/{id}/aggregates/daily`

Exact per-transaction projection completion is available at
`GET /transactions/{transaction_id}/projection-status`; the benchmark uses it
to attribute aggregate-ingestion latency to the transaction it posted.

## Benchmarks

The reproducible k6 suite, deterministic seed, profiling workflow, raw results,
and current bottleneck report live in [`benchmark/README.md`](benchmark/README.md)
and [`benchmark/REPORT.md`](benchmark/REPORT.md).

```bash
make benchmark-smoke  # 100 logical operations/s, short validation
make benchmark        # 10,000 offered logical operations/s target
```
