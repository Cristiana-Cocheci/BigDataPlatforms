# This is a deployment/installation guide


<!-- Example use for an efficient deployment

```sh
TENANTS="tenant1 tenant2" \
WORKERS=10 \
PARTITIONS=10 \
TEST_DURATION_SECONDS=120 \
PREPARE_CHUNKS=true \
RESET_STACK=true \
DRAIN_BEFORE_STOP=true \
DRAIN_TIMEOUT_SECONDS=100 \
MIN_THROUGHPUT_RPS=1000000 \
CASSANDRA_COUNT_DAY=2025-06-01 \
CASSANDRA_WRITE_SLEEP_MS=0 \
FORCE_REBUILD_IMAGES=true \
./run_benchmark.sh
``` -->


<!-- Silverpipeline guideline 1: \
go build -o batchmanager ./batchmanagercmd \
go build -o silverpipeline ./silverpipelinecmd \
./batchmanager --command extract-cache --tenant tenant2 \
./batchmanager --command status --tenant tenant2 \
./batchmanager --command run --tenant tenant2 \
./batchmanager --command status --tenant tenant2 \
./batchmanager --command cleanup-processed --tenant tenant2 \ -->

### 0) Start in code directory and build binaries if necessary

```sh
cd ./assignment-2-103803829/code
```

### 1) Ingestion benchmarking (run_benchmark.sh)

```sh
chmod +x run_benchmark.sh
```

#### 1.1 Baseline benchmark (both tenants, higher parallelism)

```sh
TENANTS="tenant1 tenant2" \
WORKERS=5 \
PARTITIONS=5 \
SOURCE_REPLICAS=5 \
TEST_DURATION_SECONDS=40 \
PREPARE_CHUNKS=true \
RESET_STACK=true \
FORCE_REBUILD_IMAGES=true \
MIN_THROUGHPUT_RPS=1000000 \
MAX_AVG_BATCH_INGEST_MS=250 \
REPORT_INTERVAL_SECONDS=10 \
CASSANDRA_COUNT_DAY=2025-06-01 \
./run_benchmark.sh
```

#### 1.2 Under-provisioned benchmark (both tenants, low parallelism)

```sh
TENANTS="tenant1 tenant2" \
WORKERS=1 \
PARTITIONS=1 \
SOURCE_REPLICAS=1 \
TEST_DURATION_SECONDS=40 \
PREPARE_CHUNKS=true \
RESET_STACK=true \
FORCE_REBUILD_IMAGES=false \
DRAIN_BEFORE_STOP=true \
DRAIN_TIMEOUT_SECONDS=120 \
REPORT_INTERVAL_SECONDS=10 \
CASSANDRA_COUNT_DAY=2025-06-01 \
./run_benchmark.sh
```

#### 1.3 Cassandra write-limit test (5ms, 20ms, 50ms, set in CASSANDRA_WRITE_SLEEP)

```sh

TENANTS="tenant1 tenant2" \
WORKERS=1 \
PARTITIONS=1 \
SOURCE_REPLICAS=1 \
TEST_DURATION_SECONDS=120 \
PREPARE_CHUNKS=true \
RESET_STACK=true \
FORCE_REBUILD_IMAGES=false \
DRAIN_BEFORE_STOP=true \
DRAIN_TIMEOUT_SECONDS=120 \
CASSANDRA_WRITE_SLEEP_MS=5 \
RESULTS_ROOT="benchmark_results/write_limit_${SLEEP_MS}ms" \
./run_benchmark.sh

```

#### 1.4 Read benchmark outputs

```sh
LATEST_RUN="$(ls -td benchmark_results/test_* | head -n 1)"
echo "$LATEST_RUN"

cat "$LATEST_RUN/run_summary.env"
cat "$LATEST_RUN/monitor_throughput_by_tenant.csv"
cat "$LATEST_RUN/cassandra_rows_by_tenant.csv"
cat "$LATEST_RUN/drain_status_by_tenant.csv"

docker exec cassandra1 cqlsh -e "CONSISTENCY ONE; SELECT COUNT(*) FROM mysimbdp_tenant2.sensor_observations_dht22_bronze WHERE day='2025-06-01' AND hour=0;"
```

### 2) Silverpipeline tests (both tenants)

#### 2.1 Prepare infrastructure and images

```sh
docker compose -f docker-compose.yml -f docker-compose.multitenant-brokers.yml up -d cassandra1 cassandra2 cassandra3
docker compose -f docker-compose.yml -f docker-compose.multitenant-brokers.yml build tenant1-silverpipeline tenant2-silverpipeline
```

#### 2.2 Full silver run (local cache) for tenant1 and tenant2

```sh
SILVER_PIPELINE_MODE=full \
SILVER_PIPELINE_DAY=2025-06-01 \
SILVER_PIPELINE_STORAGE_BACKEND=local \
docker compose --profile silver -f docker-compose.yml -f docker-compose.multitenant-brokers.yml run --rm tenant1-silverpipeline

SILVER_PIPELINE_MODE=full \
SILVER_PIPELINE_DAY=2025-06-01 \
SILVER_PIPELINE_STORAGE_BACKEND=local \
docker compose --profile silver -f docker-compose.yml -f docker-compose.multitenant-brokers.yml run --rm tenant2-silverpipeline
```

#### 2.3 Contract-mode run (extract-cache then transform-cache) for tenant1

```sh
SILVER_PIPELINE_MODE=extract-cache \
SILVER_PIPELINE_DAY=2025-06-01 \
SILVER_PIPELINE_STORAGE_BACKEND=local \
docker compose --profile silver -f docker-compose.yml -f docker-compose.multitenant-brokers.yml run --rm tenant1-silverpipeline

SILVER_PIPELINE_MODE=transform-cache \
SILVER_PIPELINE_STORAGE_BACKEND=local \
docker compose --profile silver -f docker-compose.yml -f docker-compose.multitenant-brokers.yml run --rm tenant1-silverpipeline
```

#### 2.4 Contract-mode run (extract-cache then transform-cache) for tenant2

```sh
SILVER_PIPELINE_MODE=extract-cache \
SILVER_PIPELINE_DAY=2025-06-01 \
SILVER_PIPELINE_STORAGE_BACKEND=local \
docker compose --profile silver -f docker-compose.yml -f docker-compose.multitenant-brokers.yml run --rm tenant2-silverpipeline

SILVER_PIPELINE_MODE=transform-cache \
SILVER_PIPELINE_STORAGE_BACKEND=local \
docker compose --profile silver -f docker-compose.yml -f docker-compose.multitenant-brokers.yml run --rm tenant2-silverpipeline
```

### 3) Validate silver outputs and logs

#### 3.1 Check silver row counts in Cassandra

```sh
docker exec -i cassandra1 cqlsh -e "SELECT COUNT(*) FROM mysimbdp_tenant1.sensor_measurements_bme280_silver;"
docker exec -i cassandra1 cqlsh -e "SELECT COUNT(*) FROM mysimbdp_tenant2.sensor_observations_dht22_silver;"
```

#### 3.2 Read latest silver run/task logs (both tenants)

```sh
tail -n 10 logs/silverpipeline/tenant1/run_status.jsonl
tail -n 20 logs/silverpipeline/tenant1/task_status.jsonl

tail -n 10 logs/silverpipeline/tenant2/run_status.jsonl
tail -n 20 logs/silverpipeline/tenant2/task_status.jsonl
```

### 4) Optional cleanup

```sh
docker compose -f docker-compose.yml -f docker-compose.multitenant-brokers.yml down --remove-orphans
```


### ```Benchmark Configuration``` - each of the following parameters can be overriten in the command line


```sh
TENANTS="${TENANTS:-tenant1 tenant2}"
WORKERS="${WORKERS:-1}"
PARTITIONS="${PARTITIONS:-$WORKERS}"
SOURCE_REPLICAS="${SOURCE_REPLICAS:-$WORKERS}"
SOURCE_NUM_CHUNKS="${SOURCE_NUM_CHUNKS:-$SOURCE_REPLICAS}"
SOURCE_CHUNK_AUTO_ASSIGN="${SOURCE_CHUNK_AUTO_ASSIGN:-true}"
TEST_DURATION_SECONDS="${TEST_DURATION_SECONDS:-300}"
PREPARE_CHUNKS="${PREPARE_CHUNKS:-true}"
RESET_STACK="${RESET_STACK:-true}"
FORCE_REBUILD_IMAGES="${FORCE_REBUILD_IMAGES:-false}"
STOP_BROKER_ON_STOP="${STOP_BROKER_ON_STOP:-false}"
DRAIN_BEFORE_STOP="${DRAIN_BEFORE_STOP:-true}"
DRAIN_TIMEOUT_SECONDS="${DRAIN_TIMEOUT_SECONDS:-900}"
DRAIN_POLL_INTERVAL_SECONDS="${DRAIN_POLL_INTERVAL_SECONDS:-10}"

MIN_THROUGHPUT_RPS="${MIN_THROUGHPUT_RPS:-1000000}"
MAX_AVG_BATCH_INGEST_MS="${MAX_AVG_BATCH_INGEST_MS:-250}"
ALERT_COOLDOWN_SECONDS="${ALERT_COOLDOWN_SECONDS:-15}"
REPORT_INTERVAL_SECONDS="${REPORT_INTERVAL_SECONDS:-10}"
CQLSH_REQUEST_TIMEOUT_SECONDS="${CQLSH_REQUEST_TIMEOUT_SECONDS:-180}"
POST_STOP_SETTLE_SECONDS="${POST_STOP_SETTLE_SECONDS:-15}"
CASSANDRA_COUNT_DAY="${CASSANDRA_COUNT_DAY:-2025-06-01}"
CASSANDRA_NUM_CONNS="${CASSANDRA_NUM_CONNS:-4}"
CASSANDRA_INSERT_BATCH_SIZE="${CASSANDRA_INSERT_BATCH_SIZE:-25}"
CASSANDRA_WRITE_SLEEP_MS="${CASSANDRA_WRITE_SLEEP_MS:-0}"
TENANT_CONFIG_DIR="${TENANT_CONFIG_DIR:-./tenant_configs}"
```

Explanation step by step for silver run
```sh
DAY=2025-06-01

# Build latest binaries
go build -o silverpipeline ./silverpipelinecmd
go build -o batchmanager ./batchmanagercmd

# Start clean
./batchmanager --command cleanup-processed --tenant tenant2
./batchmanager --command status --tenant tenant2

# Extract bronze cache for one day (mandatory --day)
# Use --build to ensure docker image includes latest code changes
./batchmanager --command extract-cache --tenant tenant2 --day "$DAY" --build
./batchmanager --command status --tenant tenant2

# Transform cache -> write silver to Cassandra
./batchmanager --command run --tenant tenant2
./batchmanager --command status --tenant tenant2

# Verify silver rows in Cassandra for that day
docker exec cassandra1 cqlsh -e "CONSISTENCY ONE; SELECT COUNT(*) FROM mysimbdp_tenant2.sensor_observations_dht22_silver WHERE day='${DAY}';"
docker exec cassandra1 cqlsh -e "CONSISTENCY ONE; SELECT day,hour,records_aggregated,temperature_avg,humidity_avg FROM mysimbdp_tenant2.sensor_observations_dht22_silver WHERE day='${DAY}' LIMIT 5;"

# Optional: clean cache files after test
./batchmanager --command cleanup-processed --tenant tenant2
./batchmanager --command status --tenant tenant2
```

Also for gcloud


gcloud

```sh
TENANT_ID=tenant2 \
CASSANDRA_KEYSPACE=mysimbdp_tenant2 \
CASSANDRA_HOSTS=127.0.0.1 SILVER_PIPELINE_MODE=extract-cache \
SILVER_PIPELINE_DAY=2025-06-01 \
SILVER_PIPELINE_STORAGE_BACKEND=gcs \
SILVER_PIPELINE_GCS_BUCKET=caching-silverpipeline-bucket \
SILVER_PIPELINE_GCS_PREFIX=tenant2/silverpipeline-cache SILVER_PIPELINE_GCS_CREDENTIALS_FILE=./silverpipelinecmd/css-cristianacocheci-2025-6126fecb6879.json \
go run ./silverpipelinecmd

TENANT_ID=tenant2 \
CASSANDRA_KEYSPACE=mysimbdp_tenant2 \
CASSANDRA_HOSTS=127.0.0.1 \
SILVER_PIPELINE_MODE=transform-cache \
SILVER_PIPELINE_STORAGE_BACKEND=gcs \
SILVER_PIPELINE_GCS_BUCKET=caching-silverpipeline-bucket \
SILVER_PIPELINE_GCS_PREFIX=tenant2/silverpipeline-cache \
SILVER_PIPELINE_GCS_CREDENTIALS_FILE=./silverpipelinecmd/css-cristianacocheci-2025-6126fecb6879.json \
SILVER_PIPELINE_INPUT_FILES=gs://caching-silverpipeline-bucket/tenant2/silverpipeline-cache/sensor_observations_dht22_bronze_20260311_114933_bronze_extract.csv \
go run ./silverpipelinecmd
```