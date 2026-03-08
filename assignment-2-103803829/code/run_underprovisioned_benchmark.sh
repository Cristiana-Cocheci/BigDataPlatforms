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

  local reports alerts manager_alerts
  reports="$(grep -c "report received:" "$monitor_log" 2>/dev/null || true)"
  alerts="$(grep -c "alert forwarded:" "$monitor_log" 2>/dev/null || true)"
  manager_alerts="$(grep -c "monitor alert received:" "$manager_log" 2>/dev/null || true)"

  {
    echo "reports_received=$reports"
    echo "alerts_forwarded=$alerts"
    echo "alerts_seen_by_manager=$manager_alerts"
  } >"$RUN_DIR/monitor_counters.env"

  awk '
/report received:/ {
  tenant=""; throughput=""; avg_batch="";
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
    }
  }

  if (tenant != "") {
    count[tenant]++;
    sum_throughput[tenant] += throughput;
    sum_batch[tenant] += avg_batch;
    if (!(tenant in min_throughput) || throughput < min_throughput[tenant]) {
      min_throughput[tenant] = throughput;
    }
    if (!(tenant in max_throughput) || throughput > max_throughput[tenant]) {
      max_throughput[tenant] = throughput;
    }
  }
}

END {
  print "tenant,reports,avg_throughput_rps,min_throughput_rps,max_throughput_rps,avg_batch_ingest_ms";
  for (tenant in count) {
    printf "%s,%d,%.2f,%.2f,%.2f,%.2f\n", tenant, count[tenant], sum_throughput[tenant] / count[tenant], min_throughput[tenant], max_throughput[tenant], sum_batch[tenant] / count[tenant];
  }
}
' "$monitor_log" >"$RUN_DIR/monitor_throughput_by_tenant.csv"
}

run_cqlsh_query() {
  local query="$1"
  docker exec -i cassandra1 cqlsh --request-timeout="$CQLSH_REQUEST_TIMEOUT_SECONDS" -e "$query"
}

collect_cassandra_snapshot() {
  local tenant="$1"
  local keyspace="$2"

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
  for table in $tables; do
    {
      echo "=== ${keyspace}.${table} ==="
      if ! run_cqlsh_query "SELECT COUNT(*) FROM ${keyspace}.${table};"; then
        echo "COUNT(*) failed for ${keyspace}.${table}; falling back to size estimates"
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

TENANTS="${TENANTS:-tenant1 tenant2}"
WORKERS="${WORKERS:-1}"
TEST_DURATION_SECONDS="${TEST_DURATION_SECONDS:-300}"
PREPARE_CHUNKS="${PREPARE_CHUNKS:-true}"
RESET_STACK="${RESET_STACK:-false}"
FORCE_REBUILD_IMAGES="${FORCE_REBUILD_IMAGES:-false}"
STOP_BROKER_ON_STOP="${STOP_BROKER_ON_STOP:-false}"

MIN_THROUGHPUT_RPS="${MIN_THROUGHPUT_RPS:-1000000}"
MAX_AVG_BATCH_INGEST_MS="${MAX_AVG_BATCH_INGEST_MS:-250}"
ALERT_COOLDOWN_SECONDS="${ALERT_COOLDOWN_SECONDS:-15}"
REPORT_INTERVAL_SECONDS="${REPORT_INTERVAL_SECONDS:-10}"
CQLSH_REQUEST_TIMEOUT_SECONDS="${CQLSH_REQUEST_TIMEOUT_SECONDS:-180}"
POST_STOP_SETTLE_SECONDS="${POST_STOP_SETTLE_SECONDS:-15}"

RESULTS_ROOT="${RESULTS_ROOT:-benchmark_results}"
RUN_ID="$(date +"%Y%m%d_%H%M%S")"
RUN_DIR="${RESULTS_ROOT}/underprovisioned_${RUN_ID}"
mkdir -p "$RUN_DIR"

read -r -a TENANT_LIST <<<"$TENANTS"
if [[ "${#TENANT_LIST[@]}" -eq 0 ]]; then
  echo "TENANTS is empty" >&2
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
TEST_DURATION_SECONDS=$TEST_DURATION_SECONDS
PREPARE_CHUNKS=$PREPARE_CHUNKS
RESET_STACK=$RESET_STACK
FORCE_REBUILD_IMAGES=$FORCE_REBUILD_IMAGES
STOP_BROKER_ON_STOP=$STOP_BROKER_ON_STOP
MIN_THROUGHPUT_RPS=$MIN_THROUGHPUT_RPS
MAX_AVG_BATCH_INGEST_MS=$MAX_AVG_BATCH_INGEST_MS
ALERT_COOLDOWN_SECONDS=$ALERT_COOLDOWN_SECONDS
REPORT_INTERVAL_SECONDS=$REPORT_INTERVAL_SECONDS
CQLSH_REQUEST_TIMEOUT_SECONDS=$CQLSH_REQUEST_TIMEOUT_SECONDS
POST_STOP_SETTLE_SECONDS=$POST_STOP_SETTLE_SECONDS
EOF

log "Benchmark output directory: $RUN_DIR"

if [[ "$RESET_STACK" == "true" ]]; then
  log "Resetting compose stack"
  compose down --remove-orphans || true
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

log "Waiting for cassandra1 to be ready"
if ! wait_for_container_ready cassandra1 360; then
  log "cassandra1 did not become ready within timeout"
  exit 1
fi

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
  log "Starting tenant=${tenant} workers=${WORKERS} with source producer"
  start_args=(--command start --tenant "$tenant" --workers "$WORKERS" --with-source)
  if [[ "$PREPARE_CHUNKS" == "true" ]]; then
    start_args+=(--prepare-chunks)
  fi

  ./streamingestmanager "${start_args[@]}" >"$RUN_DIR/manager_start_${tenant}.txt" 2>&1
  compose ps >"$RUN_DIR/compose_ps_after_start_${tenant}.txt" 2>&1 || true
done

log "Collecting runtime for ${TEST_DURATION_SECONDS}s"
sleep "$TEST_DURATION_SECONDS"

log "Capturing compose status and logs"
compose ps >"$RUN_DIR/compose_ps_before_stop.txt" 2>&1 || true
collect_service_logs "$INGEST_START_UTC"
collect_monitor_summary

for tenant in "${TENANT_LIST[@]}"; do
  log "Stopping tenant=${tenant} source/worker"
  stop_args=(--command stop --tenant "$tenant")
  if [[ "$STOP_BROKER_ON_STOP" == "true" ]]; then
    stop_args+=(--stop-broker)
  fi

  ./streamingestmanager "${stop_args[@]}" >"$RUN_DIR/manager_stop_${tenant}.txt" 2>&1 || true
done

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
