#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if [[ ! -x "./run_underprovisioned_benchmark.sh" ]]; then
  echo "run_underprovisioned_benchmark.sh not found or not executable" >&2
  exit 1
fi

TENANTS="${TENANTS:-tenant1 tenant2}"
WORKERS="${WORKERS:-1}"
PARTITIONS="${PARTITIONS:-1}"
TEST_DURATION_SECONDS="${TEST_DURATION_SECONDS:-120}"
PREPARE_CHUNKS="${PREPARE_CHUNKS:-true}"
RESET_STACK="${RESET_STACK:-true}"
DRAIN_BEFORE_STOP="${DRAIN_BEFORE_STOP:-true}"
DRAIN_TIMEOUT_SECONDS="${DRAIN_TIMEOUT_SECONDS:-30}"
POST_STOP_SETTLE_SECONDS="${POST_STOP_SETTLE_SECONDS:-20}"
MIN_THROUGHPUT_RPS="${MIN_THROUGHPUT_RPS:-1000000}"
MAX_AVG_BATCH_INGEST_MS="${MAX_AVG_BATCH_INGEST_MS:-250}"
REPORT_INTERVAL_SECONDS="${REPORT_INTERVAL_SECONDS:-10}"
CASSANDRA_COUNT_DAY="${CASSANDRA_COUNT_DAY:-2025-06-01}"
FORCE_REBUILD_IMAGES="${FORCE_REBUILD_IMAGES:-false}"
CASSANDRA_NUM_CONNS="${CASSANDRA_NUM_CONNS:-4}"
CASSANDRA_INSERT_BATCH_SIZE="${CASSANDRA_INSERT_BATCH_SIZE:-25}"
WRITE_SLEEP_SET="${WRITE_SLEEP_SET:-5 20 50}"
RUN_TAG="${RUN_TAG:-$(date +"%Y%m%d_%H%M%S")}" 

echo "Running Cassandra write-limit test matrix"
echo "  tenants=${TENANTS} workers=${WORKERS} partitions=${PARTITIONS} duration=${TEST_DURATION_SECONDS}s"
echo "  cassandra_num_conns=${CASSANDRA_NUM_CONNS} cassandra_insert_batch_size=${CASSANDRA_INSERT_BATCH_SIZE}"
echo "  write_sleep_set_ms=${WRITE_SLEEP_SET}"

rebuild_images="$FORCE_REBUILD_IMAGES"

for write_sleep_ms in $WRITE_SLEEP_SET; do
  if ! [[ "$write_sleep_ms" =~ ^[0-9]+$ ]]; then
    echo "Invalid write sleep value in WRITE_SLEEP_SET: ${write_sleep_ms}" >&2
    exit 1
  fi

  results_root="benchmark_results/write_limit_${write_sleep_ms}ms_${RUN_TAG}"

  echo
  echo "[matrix] starting test with CASSANDRA_WRITE_SLEEP_MS=${write_sleep_ms} RESULTS_ROOT=${results_root}"

  env \
    TENANTS="$TENANTS" \
    WORKERS="$WORKERS" \
    PARTITIONS="$PARTITIONS" \
    TEST_DURATION_SECONDS="$TEST_DURATION_SECONDS" \
    PREPARE_CHUNKS="$PREPARE_CHUNKS" \
    RESET_STACK="$RESET_STACK" \
    DRAIN_BEFORE_STOP="$DRAIN_BEFORE_STOP" \
    DRAIN_TIMEOUT_SECONDS="$DRAIN_TIMEOUT_SECONDS" \
    POST_STOP_SETTLE_SECONDS="$POST_STOP_SETTLE_SECONDS" \
    MIN_THROUGHPUT_RPS="$MIN_THROUGHPUT_RPS" \
    MAX_AVG_BATCH_INGEST_MS="$MAX_AVG_BATCH_INGEST_MS" \
    REPORT_INTERVAL_SECONDS="$REPORT_INTERVAL_SECONDS" \
    CASSANDRA_COUNT_DAY="$CASSANDRA_COUNT_DAY" \
    FORCE_REBUILD_IMAGES="$rebuild_images" \
    CASSANDRA_NUM_CONNS="$CASSANDRA_NUM_CONNS" \
    CASSANDRA_INSERT_BATCH_SIZE="$CASSANDRA_INSERT_BATCH_SIZE" \
    CASSANDRA_WRITE_SLEEP_MS="$write_sleep_ms" \
    RESULTS_ROOT="$results_root" \
    ./run_underprovisioned_benchmark.sh

  rebuild_images=false
  RESET_STACK=true

done

echo
echo "Completed write-limit matrix. Run folders are under benchmark_results/write_limit_*_${RUN_TAG}/"
