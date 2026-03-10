# Under-Provisioned Benchmarking Guide

This guide explains how to run, tune, and interpret `run_underprovisioned_benchmark.sh` for Assignment 2.

## What the script does

`run_underprovisioned_benchmark.sh` automates a heavy-ingestion scenario where workers are intentionally under-provisioned.

At a high level, it:

- starts Cassandra, Kafka, ZooKeeper, manager, and monitor
- starts tenant workers and source producers
- runs ingestion for a configurable duration
- collects logs and monitor summaries
- queries Cassandra for table discovery, counts, and sample rows
- writes all artifacts into a timestamped folder under `benchmark_results/`

## Prerequisites

Run from `assignment-2-103803829/code`.

Required commands:

- `docker`
- `go`
- `awk`
- `grep`
- `python3` only if `PREPARE_CHUNKS=true`

Files required in this folder:

- `docker-compose.yml`
- `docker-compose.multitenant-brokers.yml`
- `streamingestmanager.go`
- tenant configs under `tenant_configs/`

## First-time setup

```sh
cd assignment-2-103803829/code
chmod +x run_underprovisioned_benchmark.sh
```

## Quick runs

Single-tenant smoke benchmark:

```sh
TENANTS="tenant1" \
WORKERS=1 \
TEST_DURATION_SECONDS=120 \
MIN_THROUGHPUT_RPS=1000000 \
./run_underprovisioned_benchmark.sh
```

Two-tenant heavier benchmark:

```sh
TENANTS="tenant1 tenant2" \
WORKERS=1 \
TEST_DURATION_SECONDS=300 \
PREPARE_CHUNKS=false \
RESET_STACK=true \
MIN_THROUGHPUT_RPS=1000000 \
./run_underprovisioned_benchmark.sh
```

Custom efficient benchmark:
```sh
TENANTS="tenant1 tenant2" \
WORKERS=10 \
PARTITIONS=10 \
TEST_DURATION_SECONDS=700 \
PREPARE_CHUNKS=true \
RESET_STACK=true \
DRAIN_BEFORE_STOP=true \
DRAIN_TIMEOUT_SECONDS=100 \
MIN_THROUGHPUT_RPS=1000000 \
CASSANDRA_COUNT_DAY=2025-06-01 \
FORCE_REBUILD_IMAGES=true \
./run_underprovisioned_benchmark.sh
```

## Main parameters

All parameters are environment variables. If omitted, defaults are used.

- `TENANTS` default: `tenant1 tenant2`
- `WORKERS` default: `1`
- `PARTITIONS` default: `WORKERS`
- `TEST_DURATION_SECONDS` default: `300`
- `PREPARE_CHUNKS` default: `true`
- `RESET_STACK` default: `false`
- `FORCE_REBUILD_IMAGES` default: `false`
- `STOP_BROKER_ON_STOP` default: `false`
- `MIN_THROUGHPUT_RPS` default: `1000000`
- `MAX_AVG_BATCH_INGEST_MS` default: `250`
- `ALERT_COOLDOWN_SECONDS` default: `15`
- `REPORT_INTERVAL_SECONDS` default: `10`
- `CQLSH_REQUEST_TIMEOUT_SECONDS` default: `180`
- `POST_STOP_SETTLE_SECONDS` default: `15`
- `CASSANDRA_COUNT_DAY` default: `2025-06-01`
- `RESULTS_ROOT` default: `benchmark_results`

## Recommended tuning

To insert more data in Cassandra during the run:

- increase `TEST_DURATION_SECONDS` to `300`, `600`, or more
- keep `WORKERS=1` if you want under-provisioning behavior
- increase `POST_STOP_SETTLE_SECONDS` if Cassandra is under heavy write pressure

For cleaner reruns:

- set `RESET_STACK=true` to avoid stale state
- set `FORCE_REBUILD_IMAGES=true` after code changes

## Output folder structure

Each run creates:

- `benchmark_results/underprovisioned_<timestamp>/`

Important files:

- `run.log`: run timeline
- `test_config.env`: exact run configuration and timestamps
- `run_summary.env`: top-level counters
- `monitor_counters.env`: monitor/manager alert counters
- `monitor_throughput_by_tenant.csv`: per-tenant throughput and latency summary
- `worker_performance_lines.txt`: worker throughput snapshots
- `producer_performance_lines.txt`: producer output lines and producer performance
- `log_<service>.txt`: raw service logs
- `cassandra_tables_<tenant>.txt`: discovered tenant tables
- `cassandra_registry_<tenant>.txt`: schema profile registry rows
- `cassandra_counts_<tenant>.txt`: per-hour counts for `CASSANDRA_COUNT_DAY` (`hour=0..23`) and `total_for_day`; includes `system.size_estimates` fallback on failures
- `cassandra_samples_<tenant>.txt`: sample rows

## How to read success rate

There is no direct `success_rate` field. Use this practical metric:

- `processing_fraction = inserted_in_cassandra / produced_to_kafka`

How to compute:

1. Get produced totals from the final `total: N` lines in `producer_performance_lines.txt`.
2. Get inserted totals from Cassandra count files.
3. Divide inserted by produced per tenant.

Interpretation:

- lower fractions can be expected in short under-provisioned tests because backlog remains in Kafka
- this does not always indicate data loss

## Cassandra count behavior under load

Per-hour partition counts can still fail under heavy load. The script handles this by:

- logging count timeout/failure in `cassandra_counts_<tenant>.txt`
- falling back to `system.size_estimates`

By default, the script queries `day='2025-06-01'` for `hour=0..23` and writes `total_for_day=...`. Override day with `CASSANDRA_COUNT_DAY`.

## Troubleshooting

No monitor metrics:

- confirm monitor is running: `docker compose ... ps`
- check monitor log for `report received` lines
- check worker log for insert errors

Very low inserted counts:

- increase `TEST_DURATION_SECONDS`
- increase `POST_STOP_SETTLE_SECONDS`
- verify no worker `Consumer error` lines

Topic/broker issues:

- verify Kafka containers are healthy before start
- rerun with `RESET_STACK=true`

## Cleanup after run

Stop tenant workers/source only:

```sh
./streamingestmanager --command stop --tenant tenant1
./streamingestmanager --command stop --tenant tenant2
```

Bring down full stack:

```sh
docker compose -f docker-compose.yml -f docker-compose.multitenant-brokers.yml down --remove-orphans
```
