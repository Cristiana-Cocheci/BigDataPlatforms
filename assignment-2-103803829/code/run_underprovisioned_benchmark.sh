#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

COMPOSE_FILES=("-f" "docker-compose.yml" "-f" "docker-compose.multitenant-brokers.yml")

compose() {
  docker compose "${COMPOSE_FILES[@]}" "$@"
}

timestamp_utc() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
}

worker_service_for_tenant() {
  case "$1" in
    tenant1) echo "tenant1-streamingestworker" ;;
    tenant2) echo "tenant2-streamingestworker" ;;
    *) return 1 ;;
  esac
}

source_service_for_tenant() {
  case "$1" in
    tenant1) echo "tenant1-source" ;;
    tenant2) echo "tenant2-source" ;;
    *) return 1 ;;
  esac
}

keyspace_for_tenant() {
  case "$1" in
    tenant1) echo "mysimbdp_tenant1" ;;
    tenant2) echo "mysimbdp_tenant2" ;;
    *) return 1 ;;
  esac
}

kafka_container_for_tenant() {
  case "$1" in
    tenant1) echo "kafka-tenant1" ;;
    tenant2) echo "kafka-tenant2" ;;
    *) return 1 ;;
  esac
}

kafka_topic_for_tenant() {
  case "$1" in
    tenant1) echo "bme280-measurements" ;;
    tenant2) echo "dht22-measurements" ;;
    *) return 1 ;;
  esac
}

kafka_consumer_group_for_tenant() {
  case "$1" in
    tenant1) echo "tenant1-ingest-group" ;;
    tenant2) echo "tenant2-ingest-group" ;;
    *) return 1 ;;
  esac
}

sanitize_name() {
  printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_'
}

log() {
  local msg="$1"
  printf '[%s] %s\n' "$(date +"%H:%M:%S")" "$msg" | tee -a "$RUN_DIR/run.log"
}

wait_for_container_ready() {
  local container_name="$1"
  local timeout_seconds="$2"
  local waited=0

  while (( waited < timeout_seconds )); do
    local state
    state="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_name" 2>/dev/null || true)"

    if [[ "$state" == "healthy" || "$state" == "running" ]]; then
      return 0
    fi

    sleep 5
    waited=$((waited + 5))
  done

  return 1
}

tenant_kafka_total_lag() {
  local tenant="$1"
  local kafka_container
  local kafka_topic
  local consumer_group
  local lag_output
  local lag

  kafka_container="$(kafka_container_for_tenant "$tenant")"
  kafka_topic="$(kafka_topic_for_tenant "$tenant")"
  consumer_group="$(kafka_consumer_group_for_tenant "$tenant")"

  lag_output="$(docker exec "$kafka_container" kafka-consumer-groups --bootstrap-server localhost:29092 --describe --group "$consumer_group" --offsets 2>&1 || true)"
  lag="$(printf '%s\n' "$lag_output" | awk -v g="$consumer_group" -v t="$kafka_topic" '$1 == g && $2 == t && $6 ~ /^[0-9]+$/ {sum += $6; seen=1} END {if (seen) print sum}')"

  if [[ -n "$lag" ]]; then
    printf '%s\n' "$lag"
    return 0
  fi

  if printf '%s\n' "$lag_output" | grep -qi "does not exist"; then
    printf '0\n'
    return 0
  fi

  return 1
}

wait_for_tenant_drain() {
  local tenant="$1"
  local timeout_seconds="$2"
  local poll_interval_seconds="$3"
  local waited=0
  local lag

  while (( waited <= timeout_seconds )); do
    lag="$(tenant_kafka_total_lag "$tenant" 2>/dev/null || true)"
    if [[ -n "$lag" ]]; then
      log "Drain check tenant=${tenant} lag=${lag}"
      if [[ "$lag" -eq 0 ]]; then
        return 0
      fi
    else
      log "Drain check tenant=${tenant} lag=unknown"
    fi

    sleep "$poll_interval_seconds"
    waited=$((waited + poll_interval_seconds))
  done

  return 1
}

stop_source_only_for_tenant() {
  local tenant="$1"
  local source_service
  source_service="$(source_service_for_tenant "$tenant")"
  compose stop "$source_service"
}

remove_stopped_source_for_tenant() {
  local tenant="$1"
  local source_service
  source_service="$(source_service_for_tenant "$tenant")"
  compose rm -f "$source_service"
}

collect_service_logs() {
  local since="$1"
  local service

  for service in "${SERVICES_TO_CAPTURE[@]}"; do
    local safe_name
    safe_name="$(sanitize_name "$service")"
    compose logs --no-color --since "$since" "$service" >"$RUN_DIR/log_${safe_name}.txt" 2>&1 || true
  done
}

collect_monitor_summary() {
  local monitor_log="$RUN_DIR/log_streamingestmonitor.txt"
  local manager_log="$RUN_DIR/log_streamingestmanager.txt"

  local reports alerts manager_alerts total_ingested_mb
  reports="$(grep -c "report received:" "$monitor_log" 2>/dev/null || true)"
  alerts="$(grep -c "alert forwarded:" "$monitor_log" 2>/dev/null || true)"
  manager_alerts="$(grep -c "monitor alert received:" "$manager_log" 2>/dev/null || true)"

  awk '
/report received:/ {
  tenant=""; throughput=""; avg_batch=""; window_mb=""; total_mb="";
  for (i = 1; i <= NF; i++) {
    if ($i ~ /^tenant=/) {
      split($i, a, "=");
      tenant = a[2];
    } else if ($i ~ /^throughput=/) {
      split($i, a, "=");
      throughput = a[2] + 0;
    } else if ($i ~ /^avg_batch_ms=/) {
      split($i, a, "=");
      avg_batch = a[2] + 0;
    } else if ($i ~ /^window_mb=/) {
      split($i, a, "=");
      window_mb = a[2] + 0;
    } else if ($i ~ /^total_mb=/) {
      split($i, a, "=");
      total_mb = a[2] + 0;
    }
  }

  if (tenant != "") {
    count[tenant]++;
    sum_throughput[tenant] += throughput;
    sum_batch[tenant] += avg_batch;
    sum_window_mb[tenant] += window_mb;
    if (!(tenant in min_throughput) || throughput < min_throughput[tenant]) {
      min_throughput[tenant] = throughput;
    }
    if (!(tenant in max_throughput) || throughput > max_throughput[tenant]) {
      max_throughput[tenant] = throughput;
    }
    if (!(tenant in max_total_mb) || total_mb > max_total_mb[tenant]) {
      max_total_mb[tenant] = total_mb;
    }
  }
}

END {
  print "tenant,reports,avg_throughput_rps,min_throughput_rps,max_throughput_rps,avg_batch_ingest_ms,avg_ingested_mb_per_report,total_ingested_mb";
  for (tenant in count) {
    avg_window_mb = sum_window_mb[tenant] / count[tenant];
    tenant_total_mb = (tenant in max_total_mb) ? max_total_mb[tenant] : sum_window_mb[tenant];
    printf "%s,%d,%.2f,%.2f,%.2f,%.2f,%.4f,%.4f\n", tenant, count[tenant], sum_throughput[tenant] / count[tenant], min_throughput[tenant], max_throughput[tenant], sum_batch[tenant] / count[tenant], avg_window_mb, tenant_total_mb;
  }
}
' "$monitor_log" >"$RUN_DIR/monitor_throughput_by_tenant.csv"

  total_ingested_mb="$(awk -F, 'FNR > 1 {sum += $8} END {printf "%.4f", sum + 0}' "$RUN_DIR/monitor_throughput_by_tenant.csv" 2>/dev/null || echo "0.0000")"

  {
    echo "reports_received=$reports"
    echo "alerts_forwarded=$alerts"
    echo "alerts_seen_by_manager=$manager_alerts"
    echo "total_ingested_mb=$total_ingested_mb"
  } >"$RUN_DIR/monitor_counters.env"
}

map_container_data_path_to_host() {
  local path="$1"
  if [[ "$path" == /data/* ]]; then
    printf '../data/%s\n' "${path#/data/}"
    return
  fi

  printf '%s\n' "$path"
}

source_csv_host_path_for_tenant() {
  local tenant="$1"
  local config_path="${TENANT_CONFIG_DIR}/${tenant}.json"

  if [[ ! -f "$config_path" ]]; then
    return 1
  fi

  local source_csv
  source_csv="$(awk -F'"' '/"source_csv"[[:space:]]*:/ {print $4; exit}' "$config_path" 2>/dev/null || true)"
  if [[ -z "$source_csv" ]]; then
    return 1
  fi

  map_container_data_path_to_host "$source_csv"
}

count_csv_data_rows() {
  local csv_file="$1"

  if [[ ! -f "$csv_file" ]]; then
    return 1
  fi

  awk 'END { if (NR > 0) { print NR - 1 } else { print 0 } }' "$csv_file"
}

collect_expected_rows_by_tenant() {
  local output_file="$RUN_DIR/initial_rows_by_tenant.csv"
  local tenant

  {
    echo "tenant,initial_rows_expected_from_chunks"
    for tenant in "${TENANT_LIST[@]}"; do
      local manager_start_file="$RUN_DIR/manager_start_${tenant}.txt"
      local initial_rows=""

      if [[ -f "$manager_start_file" ]]; then
        initial_rows="$(awk -F': ' '/^Total rows:[[:space:]]*[0-9]+/ {print $2; exit}' "$manager_start_file" 2>/dev/null || true)"
      fi

      if [[ -z "$initial_rows" ]]; then
        local source_csv_host
        source_csv_host="$(source_csv_host_path_for_tenant "$tenant" 2>/dev/null || true)"
        if [[ -n "$source_csv_host" ]]; then
          initial_rows="$(count_csv_data_rows "$source_csv_host" 2>/dev/null || true)"
        fi
      fi

      if [[ -z "$initial_rows" ]]; then
        initial_rows=0
      fi

      echo "${tenant},${initial_rows}"
    done
  } >"$output_file"
}

collect_inserted_rows_by_tenant() {
  local output_file="$RUN_DIR/cassandra_rows_by_tenant.csv"
  local tenant

  {
    echo "tenant,inserted_rows_cassandra_count"
    for tenant in "${TENANT_LIST[@]}"; do
      local counts_file="$RUN_DIR/cassandra_counts_${tenant}.txt"
      local inserted_rows=0

      if [[ -f "$counts_file" ]]; then
        inserted_rows="$(awk -F= '/^total_for_day=/ {sum += ($2 + 0)} END {print sum + 0}' "$counts_file" 2>/dev/null || true)"
      fi

      if [[ -z "$inserted_rows" ]]; then
        inserted_rows=0
      fi

      echo "${tenant},${inserted_rows}"
    done
  } >"$output_file"
}

collect_producer_rows_by_tenant() {
  local output_file="$RUN_DIR/producer_rows_by_tenant.csv"
  local tenant

  {
    echo "tenant,producer_ingested_rows"
    for tenant in "${TENANT_LIST[@]}"; do
      local source_service
      local source_log
      local producer_rows=""

      source_service="$(source_service_for_tenant "$tenant")"
      source_log="$RUN_DIR/log_$(sanitize_name "$source_service").txt"

      if [[ -f "$source_log" ]]; then
        producer_rows="$(awk -F'Total messages produced: |, Total lines processed:' '/Total messages produced:/ {v = $2} END {if (v != "") print v}' "$source_log" 2>/dev/null || true)"
      fi

      if [[ -z "$producer_rows" && -f "$source_log" ]]; then
        producer_rows="$(awk -F'total: |, processed lines:' '/Produced [0-9]+ messages/ {v = $2} END {if (v != "") print v}' "$source_log" 2>/dev/null || true)"
      fi

      if [[ -z "$producer_rows" ]]; then
        producer_rows=0
      fi

      echo "${tenant},${producer_rows}"
    done
  } >"$output_file"
}

count_duplicate_rows_in_source_csv() {
  local csv_file="$1"

  if [[ ! -f "$csv_file" ]]; then
    return 1
  fi

  awk -F';' '
NR > 1 {
  sensor_id = $1;
  timestamp = $6;
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", sensor_id);
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", timestamp);

  if (timestamp == "") {
    next;
  }

  day = substr(timestamp, 1, 10);
  hour = substr(timestamp, 12, 2) + 0;
  key = day "|" hour "|" (sensor_id + 0) "|" timestamp;
  print key;
}
' "$csv_file" | LC_ALL=C sort | uniq -c | awk '{if ($1 > 1) dup += $1 - 1} END {print dup + 0}'
}

collect_duplicate_rows_by_tenant() {
  local output_file="$RUN_DIR/duplicate_rows_by_tenant.csv"
  local tenant

  {
    echo "tenant,duplicate_rows"
    for tenant in "${TENANT_LIST[@]}"; do
      local source_csv_host
      local duplicate_rows=""

      source_csv_host="$(source_csv_host_path_for_tenant "$tenant" 2>/dev/null || true)"
      if [[ -n "$source_csv_host" ]]; then
        duplicate_rows="$(count_duplicate_rows_in_source_csv "$source_csv_host" 2>/dev/null || true)"
      fi

      if [[ -z "$duplicate_rows" ]]; then
        duplicate_rows=0
      fi

      echo "${tenant},${duplicate_rows}"
    done
  } >"$output_file"
}

append_row_counts_to_monitor_summary() {
  local monitor_csv="$RUN_DIR/monitor_throughput_by_tenant.csv"
  local initial_csv="$RUN_DIR/initial_rows_by_tenant.csv"
  local inserted_csv="$RUN_DIR/cassandra_rows_by_tenant.csv"
  local producer_rows_csv="$RUN_DIR/producer_rows_by_tenant.csv"
  local duplicate_rows_csv="$RUN_DIR/duplicate_rows_by_tenant.csv"
  local insert_exception_counts_csv="$RUN_DIR/worker_insert_exception_counts.csv"
  local temp_csv="$RUN_DIR/monitor_throughput_by_tenant.tmp.csv"

  if [[ ! -f "$monitor_csv" ]]; then
    return
  fi

  awk -F, '
BEGIN {
  OFS=",";
}
FILENAME == ARGV[1] {
  if (FNR > 1) {
    initial[$1] = $2;
  }
  next;
}
FILENAME == ARGV[2] {
  if (FNR > 1) {
    inserted[$1] = $2;
  }
  next;
}
FILENAME == ARGV[3] {
  if (FNR > 1) {
    producer_rows[$1] = $2;
  }
  next;
}
FILENAME == ARGV[4] {
  if (FNR > 1) {
    duplicate_rows[$1] = $2;
  }
  next;
}
FILENAME == ARGV[5] {
  if (FNR > 1) {
    insert_exceptions[$1] = $2;
  }
  next;
}
FNR == 1 {
  print $0, "inserted_rows_cassandra_count", "initial_rows_expected_from_chunks", "producer_ingested_rows", "duplicate_rows", "insert_exception_count";
  next;
}
{
  tenant = $1;
  inserted_rows = (tenant in inserted) ? inserted[tenant] : 0;
  initial_rows = (tenant in initial) ? initial[tenant] : 0;
  produced_rows = (tenant in producer_rows) ? producer_rows[tenant] : 0;
  duplicate_count = (tenant in duplicate_rows) ? duplicate_rows[tenant] : 0;
  exception_count = (tenant in insert_exceptions) ? insert_exceptions[tenant] : 0;
  print $0, inserted_rows, initial_rows, produced_rows, duplicate_count, exception_count;
}
' "$initial_csv" "$inserted_csv" "$producer_rows_csv" "$duplicate_rows_csv" "$insert_exception_counts_csv" "$monitor_csv" >"$temp_csv"

  mv "$temp_csv" "$monitor_csv"
}

collect_insert_exception_artifacts() {
  local details_file="$RUN_DIR/worker_insert_exceptions_by_tenant.txt"
  local counts_file="$RUN_DIR/worker_insert_exception_counts.csv"
  local insert_exception_regex='failed to bind values for table|failed to insert batch into|failed to insert batch:|failed to insert final batch:|Consumer error:.*failed to insert|Consumer error:.*failed to bind'
  local tenant

  : >"$details_file"

  {
    echo "tenant,insert_exception_count"
    for tenant in "${TENANT_LIST[@]}"; do
      local worker_service
      local worker_log
      local exception_count=0

      worker_service="$(worker_service_for_tenant "$tenant")"
      worker_log="$RUN_DIR/log_$(sanitize_name "$worker_service").txt"

      {
        echo "=== tenant=${tenant} service=${worker_service} ==="
        if [[ -f "$worker_log" ]]; then
          exception_count="$(grep -Ec "$insert_exception_regex" "$worker_log" 2>/dev/null || true)"
          grep -En "$insert_exception_regex" "$worker_log" 2>/dev/null || echo "no insert exceptions found"
        else
          echo "worker log not found: $worker_log"
        fi
        echo
      } >>"$details_file"

      if [[ -z "$exception_count" ]]; then
        exception_count=0
      fi

      echo "${tenant},${exception_count}"
    done
  } >"$counts_file"
}

run_cqlsh_query() {
  local query="$1"
  docker exec -i cassandra1 cqlsh --request-timeout="$CQLSH_REQUEST_TIMEOUT_SECONDS" -e "$query"
}

collect_cassandra_snapshot() {
  local tenant="$1"
  local keyspace="$2"
  local count_day="$CASSANDRA_COUNT_DAY"

  local table_file="$RUN_DIR/cassandra_tables_${tenant}.txt"
  local registry_file="$RUN_DIR/cassandra_registry_${tenant}.txt"
  local counts_file="$RUN_DIR/cassandra_counts_${tenant}.txt"
  local samples_file="$RUN_DIR/cassandra_samples_${tenant}.txt"

  run_cqlsh_query "SELECT table_name FROM system_schema.tables WHERE keyspace_name='${keyspace}';" >"$table_file" 2>&1 || true
  run_cqlsh_query "SELECT tenant_id, schema_profile, updated_at FROM ${keyspace}.tenant_schema_registry;" >"$registry_file" 2>&1 || true

  : >"$counts_file"
  : >"$samples_file"

  local tables
  tables="$(awk '/_bronze/ {print $1}' "$table_file" | tr -d '\r')"

  if [[ -z "$tables" ]]; then
    echo "No *_bronze tables found in keyspace ${keyspace}" >>"$counts_file"
    return
  fi

  local table
  local hour
  local table_total
  local table_failed
  local partition_output
  local partition_count
  for table in $tables; do
    {
      echo "=== ${keyspace}.${table} ==="
      echo "Partition counts for day=${count_day}"
      table_total=0
      table_failed=0

      for hour in {0..23}; do
        if partition_output="$(run_cqlsh_query "SELECT COUNT(*) FROM ${keyspace}.${table} WHERE day='${count_day}' AND hour = ${hour};" 2>&1)"; then
          partition_count="$(printf '%s\n' "$partition_output" | awk '/^[[:space:]]+[0-9]+[[:space:]]*$/ {gsub(/ /, ""); print; exit}')"
          if [[ -n "$partition_count" ]]; then
            echo "hour=${hour},count=${partition_count}"
            table_total=$((table_total + partition_count))
          else
            echo "hour=${hour},count_parse_failed"
            printf '%s\n' "$partition_output"
            table_failed=1
          fi
        else
          echo "hour=${hour},count_query_failed"
          printf '%s\n' "$partition_output"
          table_failed=1
        fi
      done

      echo "total_for_day=${table_total}"
      if [[ "$table_failed" -eq 1 ]]; then
        echo "One or more partition count queries failed; falling back to size estimates"
        run_cqlsh_query "SELECT mean_partition_size, partitions_count FROM system.size_estimates WHERE keyspace_name='${keyspace}' AND table_name='${table}';" || true
      fi
      echo
    } >>"$counts_file" 2>&1

    {
      echo "=== ${keyspace}.${table} (LIMIT 5) ==="
      run_cqlsh_query "SELECT * FROM ${keyspace}.${table} LIMIT 5;" || true
      echo
    } >>"$samples_file" 2>&1
  done
}

require_cmd docker
require_cmd go
require_cmd awk
require_cmd grep
require_cmd sort
require_cmd uniq

TENANTS="${TENANTS:-tenant1 tenant2}"
WORKERS="${WORKERS:-1}"
PARTITIONS="${PARTITIONS:-$WORKERS}"
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
TENANT_CONFIG_DIR="${TENANT_CONFIG_DIR:-./tenant_configs}"

RESULTS_ROOT="${RESULTS_ROOT:-benchmark_results}"
RUN_ID="$(date +"%Y%m%d_%H%M%S")"
RUN_DIR="${RESULTS_ROOT}/test_${RUN_ID}"
mkdir -p "$RUN_DIR"

read -r -a TENANT_LIST <<<"$TENANTS"
if [[ "${#TENANT_LIST[@]}" -eq 0 ]]; then
  echo "TENANTS is empty" >&2
  exit 1
fi

if ! [[ "$PARTITIONS" =~ ^[0-9]+$ ]] || [[ "$PARTITIONS" -lt 1 ]]; then
  echo "PARTITIONS must be an integer >= 1" >&2
  exit 1
fi

for tenant in "${TENANT_LIST[@]}"; do
  worker_service_for_tenant "$tenant" >/dev/null
  source_service_for_tenant "$tenant" >/dev/null
  keyspace_for_tenant "$tenant" >/dev/null
done

if [[ "$PREPARE_CHUNKS" == "true" ]]; then
  require_cmd python3
fi

SERVICES_TO_CAPTURE=("streamingestmanager" "streamingestmonitor")
for tenant in "${TENANT_LIST[@]}"; do
  SERVICES_TO_CAPTURE+=("$(worker_service_for_tenant "$tenant")")
  SERVICES_TO_CAPTURE+=("$(source_service_for_tenant "$tenant")")
done

cat >"$RUN_DIR/test_config.env" <<EOF
RUN_ID=$RUN_ID
STARTED_AT_UTC=$(timestamp_utc)
TENANTS=$TENANTS
WORKERS=$WORKERS
PARTITIONS=$PARTITIONS
TEST_DURATION_SECONDS=$TEST_DURATION_SECONDS
PREPARE_CHUNKS=$PREPARE_CHUNKS
RESET_STACK=$RESET_STACK
FORCE_REBUILD_IMAGES=$FORCE_REBUILD_IMAGES
STOP_BROKER_ON_STOP=$STOP_BROKER_ON_STOP
DRAIN_BEFORE_STOP=$DRAIN_BEFORE_STOP
DRAIN_TIMEOUT_SECONDS=$DRAIN_TIMEOUT_SECONDS
DRAIN_POLL_INTERVAL_SECONDS=$DRAIN_POLL_INTERVAL_SECONDS
MIN_THROUGHPUT_RPS=$MIN_THROUGHPUT_RPS
MAX_AVG_BATCH_INGEST_MS=$MAX_AVG_BATCH_INGEST_MS
ALERT_COOLDOWN_SECONDS=$ALERT_COOLDOWN_SECONDS
REPORT_INTERVAL_SECONDS=$REPORT_INTERVAL_SECONDS
CQLSH_REQUEST_TIMEOUT_SECONDS=$CQLSH_REQUEST_TIMEOUT_SECONDS
POST_STOP_SETTLE_SECONDS=$POST_STOP_SETTLE_SECONDS
CASSANDRA_COUNT_DAY=$CASSANDRA_COUNT_DAY
EOF

log "Benchmark output directory: $RUN_DIR"

if [[ "$RESET_STACK" == "true" ]]; then
  log "Resetting compose stack (including volumes)"
  compose down --remove-orphans -v || true
fi

if [[ "$FORCE_REBUILD_IMAGES" == "true" ]]; then
  log "Rebuilding docker images"
  build_services=("streamingestmanager" "streamingestmonitor")
  for tenant in "${TENANT_LIST[@]}"; do
    build_services+=("$(source_service_for_tenant "$tenant")")
    build_services+=("$(worker_service_for_tenant "$tenant")")
  done

  compose build "${build_services[@]}"
fi

log "Building manager binary"
go build -o streamingestmanager streamingestmanager.go

export MONITOR_MIN_THROUGHPUT_RPS="$MIN_THROUGHPUT_RPS"
export MONITOR_MAX_AVG_BATCH_INGEST_MS="$MAX_AVG_BATCH_INGEST_MS"
export MONITOR_ALERT_COOLDOWN_SECONDS="$ALERT_COOLDOWN_SECONDS"
export MONITOR_REPORT_INTERVAL_SECONDS="$REPORT_INTERVAL_SECONDS"

log "Starting infrastructure and control services"
compose up -d cassandra1 cassandra2 cassandra3 zookeeper-tenant1 kafka-tenant1 zookeeper-tenant2 kafka-tenant2 streamingestmanager
compose up -d --force-recreate streamingestmonitor

for cassandra_node in cassandra1 cassandra2 cassandra3; do
  log "Waiting for ${cassandra_node} to be ready"
  if ! wait_for_container_ready "$cassandra_node" 360; then
    log "${cassandra_node} did not become ready within timeout"
    exit 1
  fi
done

for tenant in "${TENANT_LIST[@]}"; do
  kafka_container="$(kafka_container_for_tenant "$tenant")"
  log "Waiting for ${kafka_container} to be ready"
  if ! wait_for_container_ready "$kafka_container" 240; then
    log "${kafka_container} did not become ready within timeout"
    exit 1
  fi
done

INGEST_START_UTC="$(timestamp_utc)"
echo "INGEST_START_UTC=$INGEST_START_UTC" >>"$RUN_DIR/test_config.env"

for tenant in "${TENANT_LIST[@]}"; do
  log "Starting tenant=${tenant} workers=${WORKERS} partitions=${PARTITIONS} with source producer"
  start_args=(--command start --tenant "$tenant" --workers "$WORKERS" --partitions "$PARTITIONS" --with-source)
  if [[ "$PREPARE_CHUNKS" == "true" ]]; then
    start_args+=(--prepare-chunks)
  fi

  ./streamingestmanager "${start_args[@]}" >"$RUN_DIR/manager_start_${tenant}.txt" 2>&1
  compose ps >"$RUN_DIR/compose_ps_after_start_${tenant}.txt" 2>&1 || true
done

log "Collecting runtime for ${TEST_DURATION_SECONDS}s"
sleep "$TEST_DURATION_SECONDS"

log "Capturing compose status before shutdown"
compose ps >"$RUN_DIR/compose_ps_before_stop.txt" 2>&1 || true

echo "tenant,drain_status,final_kafka_lag" >"$RUN_DIR/drain_status_by_tenant.csv"

if [[ "$DRAIN_BEFORE_STOP" == "true" ]]; then
  log "Drain-aware shutdown enabled: stopping sources before draining workers"

  for tenant in "${TENANT_LIST[@]}"; do
    log "Stopping source for tenant=${tenant} before drain"
    stop_source_only_for_tenant "$tenant" >"$RUN_DIR/source_stop_${tenant}.txt" 2>&1 || true
  done

  for tenant in "${TENANT_LIST[@]}"; do
    local_drain_status="timeout"
    final_lag="-1"

    log "Waiting for tenant=${tenant} lag to drain (timeout=${DRAIN_TIMEOUT_SECONDS}s)"
    if wait_for_tenant_drain "$tenant" "$DRAIN_TIMEOUT_SECONDS" "$DRAIN_POLL_INTERVAL_SECONDS"; then
      local_drain_status="drained"
    fi

    final_lag="$(tenant_kafka_total_lag "$tenant" 2>/dev/null || true)"
    if [[ -z "$final_lag" ]]; then
      final_lag=-1
    fi

    echo "${tenant},${local_drain_status},${final_lag}" >>"$RUN_DIR/drain_status_by_tenant.csv"
  done

  log "Capturing logs and monitor summary after drain wait"
  collect_service_logs "$INGEST_START_UTC"
  collect_monitor_summary

  for tenant in "${TENANT_LIST[@]}"; do
    log "Stopping tenant=${tenant} worker after drain"
    stop_args=(--command stop --tenant "$tenant" --stop-source=false)
    if [[ "$STOP_BROKER_ON_STOP" == "true" ]]; then
      stop_args+=(--stop-broker)
    fi

    ./streamingestmanager "${stop_args[@]}" >"$RUN_DIR/manager_stop_${tenant}.txt" 2>&1 || true
    remove_stopped_source_for_tenant "$tenant" >>"$RUN_DIR/source_stop_${tenant}.txt" 2>&1 || true
  done
else
  log "Drain-aware shutdown disabled: capturing logs and stopping source/worker immediately"
  collect_service_logs "$INGEST_START_UTC"
  collect_monitor_summary

  for tenant in "${TENANT_LIST[@]}"; do
    echo "${tenant},disabled,-1" >>"$RUN_DIR/drain_status_by_tenant.csv"
    log "Stopping tenant=${tenant} source/worker"
    stop_args=(--command stop --tenant "$tenant")
    if [[ "$STOP_BROKER_ON_STOP" == "true" ]]; then
      stop_args+=(--stop-broker)
    fi

    ./streamingestmanager "${stop_args[@]}" >"$RUN_DIR/manager_stop_${tenant}.txt" 2>&1 || true
  done
fi

if [[ "$POST_STOP_SETTLE_SECONDS" -gt 0 ]]; then
  log "Waiting ${POST_STOP_SETTLE_SECONDS}s for Cassandra write pressure to settle"
  sleep "$POST_STOP_SETTLE_SECONDS"
fi

compose ps >"$RUN_DIR/compose_ps_final.txt" 2>&1 || true
./streamingestmanager --command status >"$RUN_DIR/manager_status_final.txt" 2>&1 || true

for tenant in "${TENANT_LIST[@]}"; do
  keyspace="$(keyspace_for_tenant "$tenant")"
  log "Collecting Cassandra snapshot for tenant=${tenant} keyspace=${keyspace}"
  collect_cassandra_snapshot "$tenant" "$keyspace"
done

log "Computing expected, produced, inserted, duplicate, and exception row metrics by tenant"
collect_expected_rows_by_tenant
collect_producer_rows_by_tenant
collect_inserted_rows_by_tenant
collect_duplicate_rows_by_tenant
collect_insert_exception_artifacts
append_row_counts_to_monitor_summary

log "Extracting worker and producer performance lines"
: >"$RUN_DIR/worker_performance_lines.txt"
: >"$RUN_DIR/producer_performance_lines.txt"

for tenant in "${TENANT_LIST[@]}"; do
  worker_service="$(worker_service_for_tenant "$tenant")"
  source_service="$(source_service_for_tenant "$tenant")"

  grep -E "Throughput:|Performance: Duration=" "$RUN_DIR/log_$(sanitize_name "$worker_service").txt" >>"$RUN_DIR/worker_performance_lines.txt" 2>/dev/null || true
  grep -E "Produced [0-9]+ messages|Performance: Duration=" "$RUN_DIR/log_$(sanitize_name "$source_service").txt" >>"$RUN_DIR/producer_performance_lines.txt" 2>/dev/null || true
done

FINISHED_AT_UTC="$(timestamp_utc)"
echo "FINISHED_AT_UTC=$FINISHED_AT_UTC" >>"$RUN_DIR/test_config.env"

{
  echo "run_dir=$RUN_DIR"
  echo "finished_at_utc=$FINISHED_AT_UTC"
  cat "$RUN_DIR/monitor_counters.env"
} >"$RUN_DIR/run_summary.env"

log "Benchmark completed. Main outputs:"
log "- $RUN_DIR/run_summary.env"
log "- $RUN_DIR/monitor_throughput_by_tenant.csv"
log "- $RUN_DIR/worker_performance_lines.txt"
log "- $RUN_DIR/producer_performance_lines.txt"
log "- $RUN_DIR/cassandra_counts_<tenant>.txt"
log "- $RUN_DIR/cassandra_samples_<tenant>.txt"
log "- $RUN_DIR/initial_rows_by_tenant.csv"
log "- $RUN_DIR/producer_rows_by_tenant.csv"
log "- $RUN_DIR/cassandra_rows_by_tenant.csv"
log "- $RUN_DIR/duplicate_rows_by_tenant.csv"
log "- $RUN_DIR/drain_status_by_tenant.csv"
log "- $RUN_DIR/worker_insert_exception_counts.csv"
log "- $RUN_DIR/worker_insert_exceptions_by_tenant.txt"
