# Assignment 2 code

This implementation supports local multi-tenant ingestion with:

- One Kafka broker per tenant
- One `streamingestworker` service per tenant (scalable replicas)
- One control component: `mysimbdp-streamingestmanager`
- Shared Cassandra cluster, isolated tenant keyspaces

The active standalone deployment for assignment 2 is:

- `docker-compose.yml` (shared Cassandra cluster)
- `docker-compose.multitenant-brokers.yml` (tenant Kafka/ZooKeeper, sources, workers)

## Is separate Kafka per tenant possible?

Yes. This repo includes `docker-compose.multitenant-brokers.yml` where each tenant has its own ZooKeeper + Kafka pair:

- `tenant1`: `zookeeper-tenant1`, `kafka-tenant1`
- `tenant2`: `zookeeper-tenant2`, `kafka-tenant2`

Both tenants can ingest independently by running `tenant1-source` and `tenant2-source`, with tenant-specific source files and parsing rules defined in `tenant_configs/*.json`.

In the current setup, each tenant can also use a different CSV structure and source file through `tenant_configs/*.json`.

## mysimbdp-streamingestmanager

`mysimbdp-streamingestmanager` is implemented as a CLI component in `streamingestmanager.go`.

### Responsibilities

For each tenant, it can:

1. Start required infra (`cassandra` + tenant Kafka/ZooKeeper)
2. Ensure tenant keyspace exists
3. Ensure tenant topic exists
4. Start/scale `streamingestworker` instances on-demand
5. Stop worker/source instances and optionally stop tenant broker
6. Show tenant or global status
7. Receive performance alerts from `mysimbdp-streamingestmonitor`

### Commands

```sh
# build manager
go build -o streamingestmanager streamingestmanager.go

# run manager alert receiver (used by streamingestmonitor)
./streamingestmanager --command listen-alerts --alert-listen-addr :8082

# start tenant workers
./streamingestmanager --command start --tenant tenant1 --workers 2

# start tenant workers + source simulator
./streamingestmanager --command start --tenant tenant2 --workers 3 --with-source

# prepare tenant chunks with num_chunks=workers, then start
./streamingestmanager --command start --tenant tenant2 --workers 3 --with-source --prepare-chunks

# check status (all tenants)
./streamingestmanager --command status

# stop tenant workers/source (keep broker running)
./streamingestmanager --command stop --tenant tenant1

# stop tenant workers/source and tenant broker stack
./streamingestmanager --command stop --tenant tenant2 --stop-broker
```

## Observability component (`mysimbdp-streamingestmonitor`)

The observability path has 3 parts:

1. `mysimbdp-streamingestworker` (consumer) periodically emits performance reports.
2. `mysimbdp-streamingestmonitor` receives reports and checks thresholds.
3. `mysimbdp-streamingestmonitor` informs `mysimbdp-streamingestmanager` when performance is below thresholds.

### Report format (worker -> monitor)

Workers send `POST` requests to `/reports` with JSON payload:

```json
{
  "tenant_id": "tenant1",
  "worker_id": "code-tenant1-streamingestworker-1",
  "kafka_topic": "bme280-measurements",
  "reported_at": "2026-03-07T16:40:00Z",
  "window_seconds": 15.0,
  "records_in_window": 4200,
  "batches_in_window": 168,
  "avg_batch_ingest_ms": 12.7,
  "throughput_records_per_sec": 280.0,
  "ingested_bytes_in_window": 1543200,
  "ingested_mb_in_window": 1.4717,
  "ingested_mb_per_sec": 0.0981,
  "total_ingested_bytes": 58100000,
  "total_ingested_mb": 55.4085,
  "total_inserted": 158000,
  "total_consumed": 158500
}
```

### Alert format (monitor -> manager)

When thresholds are violated, monitor sends `POST /alerts` to manager:

```json
{
  "tenant_id": "tenant1",
  "worker_id": "code-tenant1-streamingestworker-1",
  "triggered_at": "2026-03-07T16:40:05Z",
  "severity": "warning",
  "reasons": [
    "throughput 280.00 rps below minimum 300.00 rps"
  ],
  "thresholds": {
    "min_throughput_rps": 300.0,
    "max_avg_batch_ingest_ms": 250.0
  },
  "report": {
    "tenant_id": "tenant1",
    "worker_id": "code-tenant1-streamingestworker-1",
    "kafka_topic": "bme280-measurements",
    "reported_at": "2026-03-07T16:40:00Z",
    "window_seconds": 15.0,
    "records_in_window": 4200,
    "batches_in_window": 168,
    "avg_batch_ingest_ms": 12.7,
    "throughput_records_per_sec": 280.0,
    "ingested_bytes_in_window": 1543200,
    "ingested_mb_in_window": 1.4717,
    "ingested_mb_per_sec": 0.0981,
    "total_ingested_bytes": 58100000,
    "total_ingested_mb": 55.4085,
    "total_inserted": 158000,
    "total_consumed": 158500
  }
}
```

### Threshold and reporting mechanism

- Worker report destination: `MONITOR_REPORT_URL` (default `http://streamingestmonitor:8081/reports`)
- Report interval: `MONITOR_REPORT_INTERVAL_SECONDS` (default `15`)
- Monitor threshold: `MONITOR_MIN_THROUGHPUT_RPS` (default `300`)
- Monitor threshold: `MONITOR_MAX_AVG_BATCH_INGEST_MS` (default `250`)
- Alert cooldown per tenant: `MONITOR_ALERT_COOLDOWN_SECONDS` (default `30`)
- Manager alert endpoint: `POST /alerts` (listens on `:8082`)

### Demonstration commands

```sh
# from code/
go build -o streamingestmanager streamingestmanager.go
./streamingestmanager --command start --tenant tenant1 --workers 2 --with-source --prepare-chunks

# observe worker reports and monitor decisions
docker compose -f docker-compose.yml -f docker-compose.multitenant-brokers.yml logs -f streamingestmonitor streamingestmanager

# force alerts for demo by setting strict throughput threshold and recreating monitor
MONITOR_MIN_THROUGHPUT_RPS=100000 \
docker compose -f docker-compose.yml -f docker-compose.multitenant-brokers.yml up -d --force-recreate streamingestmonitor
```

Expected behavior in logs:

- `streamingestmonitor`: `report received ...`
- `streamingestmonitor`: `alert forwarded ...`
- `streamingestmanager`: `monitor alert received ...`

`report received` and `monitor alert received` log lines also include `window_mb=...` and `total_mb=...` for data-volume tracking.

## Tenant2 silver pipeline

`silverpipelinecmd/main.go` implements the tenant2 silver transformation stage for question 2.

Behavior:

- discovers tenant bronze tables matching `<table_prefix>_*_bronze` in the tenant keyspace
- extracts bronze rows from Cassandra to the configured cache backend (`local` filesystem or `gcs` bucket objects)
- reloads cached bronze CSVs from the same backend, drops rows with missing fields, and computes hourly `avg`, `min`, `max`, and `median`
- writes silver aggregates back to Cassandra tables named `<table_prefix>_<suffix>_silver`
- writes hourly silver summary CSVs to the configured cache backend

The tenant2 implementation is intentionally tenant-specific. It validates `TENANT_ID=tenant2` and reads its keyspace/table settings from `tenant_configs/tenant2.json` and `tenant_configs/silverpipeline_tenant2.yaml`.

For tenant2, the runtime config is in `tenant_configs/silverpipeline_tenant2.yaml` and the hourly metrics are computed for:

- `temperature`
- `humidity`

### Build and run

```sh
# build the silver pipeline binary
go build -o silverpipeline ./silverpipelinecmd

# run local-cache mode (default backend)
TENANT_ID=tenant2 \
CASSANDRA_KEYSPACE=mysimbdp_tenant2 \
CASSANDRA_HOSTS=cassandra1,cassandra2,cassandra3 \
SILVER_PIPELINE_DAY=2025-06-01 \
./silverpipeline

# run GCS-cache mode
TENANT_ID=tenant2 \
CASSANDRA_KEYSPACE=mysimbdp_tenant2 \
CASSANDRA_HOSTS=cassandra1,cassandra2,cassandra3 \
SILVER_PIPELINE_STORAGE_BACKEND=gcs \
SILVER_PIPELINE_GCS_BUCKET=caching-silverpipeline-bucket \
SILVER_PIPELINE_GCS_PREFIX=tenant2/silverpipeline-cache \
SILVER_PIPELINE_GCS_CREDENTIALS_FILE=./silverpipelinecmd/css-cristianacocheci-2025-6126fecb6879.json \
SILVER_PIPELINE_DAY=2025-06-01 \
./silverpipeline
```

Or run it with Docker Compose after bronze ingestion has completed:

```sh
docker compose --profile silver -f docker-compose.yml -f docker-compose.multitenant-brokers.yml run --rm tenant2-silverpipeline
```

Black-box contract used by `mysimbdp-batchmanager` and direct runs:

- `SILVER_PIPELINE_MODE=extract-cache` extracts bronze Cassandra tables into the configured backend
- `SILVER_PIPELINE_DAY=YYYY-MM-DD` scopes bronze extraction to that day partition (`day`)
- `SILVER_PIPELINE_MODE=transform-cache` transforms cached bronze CSVs and writes tenant2 silver tables
- `SILVER_PIPELINE_INPUT_FILES=file1.csv,file2.csv` limits `transform-cache` mode to specific cache inputs
- `SILVER_PIPELINE_STORAGE_BACKEND=local|gcs` selects the cache backend (`local` by default)
- `SILVER_PIPELINE_GCS_BUCKET`, `SILVER_PIPELINE_GCS_PREFIX`, and `SILVER_PIPELINE_GCS_CREDENTIALS_FILE` configure GCS mode
- `SILVER_PIPELINE_LOG_DIR` sets the base path for silverpipeline file logs
- `SILVER_PIPELINE_RUN_LOG_FILE` sets the run-status JSONL filename/path
- `SILVER_PIPELINE_TASK_LOG_FILE` sets the task-status JSONL filename/path

Expected outputs:

- local backend: bronze and silver summary CSVs in `tenant_caching_dir/tenant2`
- GCS backend: bronze and silver summary CSVs in `gs://<bucket>/<prefix>/...`
- Cassandra silver table for tenant2: `mysimbdp_tenant2.sensor_observations_dht22_silver`
- run log file (JSONL): default `logs/silverpipeline/tenant2/run_status.jsonl`
- task log file (JSONL): default `logs/silverpipeline/tenant2/task_status.jsonl`

## mysimbdp-batchmanager

`batchmanagercmd/main.go` implements a provider-side batch control component that treats `tenant2-silverpipeline` as a black box.

Behavior:

- scans `tenant_caching_dir/tenant2` for bronze cache files matching `*_bronze_extract.csv`
- tracks both bronze (`*_bronze_extract.csv`) and silver (`*_silver_hourly.csv`) cache files in `tenant_caching_dir/tenant2/.batchmanager_state.json`
- requires `--day YYYY-MM-DD` for `--command extract-cache`
- deletes all bronze and silver cache files with `--command cleanup-processed` while recording each deleted file in the batchmanager state
- invokes the tenant pipeline through `docker compose run --rm tenant2-silverpipeline`
- passes only contract-level inputs (`SILVER_PIPELINE_MODE`, `SILVER_PIPELINE_DAY`, and `SILVER_PIPELINE_INPUT_FILES`) instead of inspecting pipeline internals

Current limitation:
`mysimbdp-batchmanager` tracks only local filesystem cache files. For `storage_backend: gcs`, run the tenant2 silverpipeline directly (`SILVER_PIPELINE_MODE=full|extract-cache|transform-cache`) instead of using batchmanager state tracking.

### Build and run

```sh
# build batchmanager
go build -o batchmanager ./batchmanagercmd

# optional: refresh tenant2 cache from bronze Cassandra tables
./batchmanager --command extract-cache --tenant tenant2 --day 2025-06-01

# inspect pending cache files
./batchmanager --command status --tenant tenant2

# process pending cached files through the tenant2 black-box silverpipeline
./batchmanager --command run --tenant tenant2

# delete all bronze and silver cache files for tenant2
./batchmanager --command cleanup-processed --tenant tenant2
```

## Black-box model for streamingestworker

`mysimbdp` imposes an invocation contract. The manager does not inspect worker code; it only starts/stops container instances with agreed runtime parameters.

### Contract (required when `WORKER_MODE=managed`)

The worker must read configuration from environment variables:

- `TENANT_ID`
- `KAFKA_BROKERS`
- `KAFKA_TOPIC`
- `KAFKA_CONSUMER_GROUP`
- `CASSANDRA_KEYSPACE`
- `CASSANDRA_HOSTS`

The current `consumer.go` enforces this in managed mode and fails fast when required variables are missing.

## Tenant configuration folder

Tenant-specific behavior is centralized in:

- `tenant_configs/tenant1.json`
- `tenant_configs/tenant2.json`

Each file defines:

- `csv_format` (parsing rules in producer)
- `table_prefix` + `schema_profile` (high-level schema identity)
- `schema.table_suffix_field` (payload field used to derive per-sensor/per-device table suffix)
- `schema.columns[]` (column name/type and mapping to measurement fields)
- `schema.primary_key.partition[]` + `schema.primary_key.clustering[]`
- `source_csv` + `source_chunk_dir` (source input paths)

`consumer.go` no longer has tenant-specific hardcoded insert statements. It now builds `CREATE TABLE` and `INSERT` CQL directly from each tenant JSON schema.
Kafka payloads are decoded as generic JSON maps, and values are cast according to each configured CQL column type.

Supported `csv_format` values:

- `bme280_full` → `sensor_id;sensor_type;location;lat;lon;timestamp;pressure;altitude;pressure_sealevel;temperature;humidity`
- `dht22_compact` → `sensor_id;sensor_type;location;lat;lon;timestamp;temperature;humidity`

## Tenant-specific tables and datatypes

The worker uses tenant JSON schema directly for table DDL and inserts.

- Column set and column datatypes are read from `schema.columns`
- Measurement-to-column mapping is read from `schema.columns[].field`
- Primary key structure is read from `schema.primary_key`
- Table suffix source field is read from `schema.table_suffix_field`
- Table prefix remains tenant-specific via `table_prefix`

Tables are generated dynamically from the configured `schema.table_suffix_field` value, and table names are sanitized before execution.

When starting a tenant, `mysimbdp-streamingestmanager` also bootstraps a keyspace-local registry table:

- `<tenant_keyspace>.tenant_schema_registry(tenant_id, schema_profile, updated_at)`

This makes the active schema profile explicit per tenant keyspace.

### Execution steps

1. Tenant (or platform operator) asks manager to start workers.
2. Manager resolves tenant registry entry (`tenant -> broker, topic, keyspace, services`).
3. Manager ensures broker/topic/keyspace exist.
4. Manager starts/scales worker containers.
5. Worker consumes tenant stream and writes only to tenant keyspace.
6. Manager can stop replicas/broker on-demand for pay-per-use.

## What tenant must do to provide a compatible streamingestworker

To make a tenant-developed worker work with `mysimbdp-streamingestmanager`, tenant must:

1. Package worker as a container runnable without interactive input.
2. Implement configuration only via the contract env vars listed above.
3. Consume from `KAFKA_TOPIC` at `KAFKA_BROKERS` with `KAFKA_CONSUMER_GROUP`.
4. Write to `CASSANDRA_KEYSPACE` on `CASSANDRA_HOSTS`.
5. Exit non-zero on unrecoverable startup/config errors; keep running otherwise.
6. Emit logs to stdout/stderr so manager/operator can observe behavior.

## Local run (2 tenants, separate Kafka brokers)

```sh
# from this code directory
docker compose -f docker-compose.yml -f docker-compose.multitenant-brokers.yml up -d \
  cassandra1 cassandra2 cassandra3 zookeeper-tenant1 kafka-tenant1 zookeeper-tenant2 kafka-tenant2

# optional: create keyspaces manually (manager also auto-creates on start)
docker exec -i cassandra1 cqlsh < init_multitenant.cql

# build manager and start workers/sources
go build -o streamingestmanager streamingestmanager.go
./streamingestmanager --command start --tenant tenant1 --workers 5 --with-source
./streamingestmanager --command start --tenant tenant2 --workers 5 --with-source

# optional: regenerate chunks from source CSVs using workers count
./streamingestmanager --command start --tenant tenant1 --workers 5 --with-source --prepare-chunks
./streamingestmanager --command start --tenant tenant2 --workers 5 --with-source --prepare-chunks

# verify
./streamingestmanager --command status
docker exec -it cassandra1 cqlsh -e "SELECT * FROM mysimbdp_tenant1.sensor_measurements_bme280_bronze LIMIT 5;"
docker exec -it cassandra1 cqlsh -e "SELECT * FROM mysimbdp_tenant2.sensor_observations_dht22_bronze LIMIT 5;"
```

## Heavy under-provisioned benchmark script

Use `run_underprovisioned_benchmark.sh` to demonstrate intensive ingestion with intentionally limited worker capacity, while still running source producers.

Detailed step-by-step instructions are available in `BENCHMARKING_GUIDE.md`.

The script automatically:

- starts infra + manager + monitor
- starts tenant workers with `--with-source` (and optional chunk preparation)
- keeps ingestion running for a fixed duration
- captures monitor/manager/worker/source logs into timestamped files
- extracts throughput and alert counters
- checks Cassandra content at the end (table discovery, counts, sample rows)

Quick run (single tenant, under-provisioned):

```sh
cd code
chmod +x run_underprovisioned_benchmark.sh

TENANTS="tenant1" \
WORKERS=1 \
TEST_DURATION_SECONDS=120 \
MIN_THROUGHPUT_RPS=1000000 \
./run_underprovisioned_benchmark.sh
```

Heavier run (both tenants, same worker limit):

```sh
cd code
TENANTS="tenant1 tenant2" \
WORKERS=1 \
TEST_DURATION_SECONDS=300 \
PREPARE_CHUNKS=false \
MIN_THROUGHPUT_RPS=1000000 \
./run_underprovisioned_benchmark.sh
```

Notes:

- `TEST_DURATION_SECONDS` is configurable and defaults to `300` in the script.
- Increase `TEST_DURATION_SECONDS` further (for example `600`) when you want more records to be inserted before stopping.
- `CASSANDRA_COUNT_DAY` controls which day is queried for per-hour Cassandra counts (default: `2025-06-01`).

Main output folder:

- `benchmark_results/underprovisioned_<timestamp>/`

Important files inside each run folder:

- `run_summary.env`: top-level counters and run metadata
- `monitor_throughput_by_tenant.csv`: per-tenant throughput/latency summary from monitor logs, including ingested MB metrics
- `worker_performance_lines.txt`: worker-level `Performance: Duration... Throughput...` lines
- `producer_performance_lines.txt`: producer-side throughput lines
- `cassandra_tables_<tenant>.txt`: discovered Cassandra tables in tenant keyspace
- `cassandra_counts_<tenant>.txt`: per-hour counts for `CASSANDRA_COUNT_DAY` (`hour=0..23`) and `total_for_day`, with fallback to `system.size_estimates` if needed
- `cassandra_samples_<tenant>.txt`: `SELECT ... LIMIT 5` sample rows for each `*_bronze` table
