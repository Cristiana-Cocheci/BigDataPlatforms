# This is a deployment/installation guide

Running a benchmark

```Configuration``` - each of the following parameters can be overriten in the command line


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

Example use for an efficient deployment

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
```


Silverpipeline guideline 1:
go build -o batchmanager ./batchmanagercmd
go build -o silverpipeline ./silverpipelinecmd
./batchmanager --command extract-cache --tenant tenant2
./batchmanager --command status --tenant tenant2
./batchmanager --command run --tenant tenant2
./batchmanager --command status --tenant tenant2
./batchmanager --command cleanup-processed --tenant tenant2


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