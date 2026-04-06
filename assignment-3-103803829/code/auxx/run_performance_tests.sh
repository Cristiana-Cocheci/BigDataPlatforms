#!/bin/bash
# Performance Test Harness for Stream Analytics Application
#
# Runs all performance scenarios (A1-A3, B1-B4) and collects metrics
# Usage: ./auxx/run_performance_tests.sh [--skip-infra] [--only=A1,A2,A3]
#
# Sets: JAVA_HOME, activates venv, starts infrastructure, runs tests with timing

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
RESULTS_DIR="$PROJECT_ROOT/results"
LOGS_DIR="$PROJECT_ROOT/logs"

# Configuration
JAVA_HOME="${JAVA_HOME:-/opt/homebrew/opt/openjdk@11/libexec/openjdk.jdk/Contents/Home}"
KAFKA_BROKERS="127.0.0.1:9094"
KAFKA_TOPIC="dht22-measurements"
CASSANDRA_HOSTS="127.0.0.1"
CASSANDRA_KEYSPACE="mysimbdp_tenant2"
TIMEOUT_WARNED=0

run_with_timeout() {
    local seconds="$1"
    shift

    if command -v timeout >/dev/null 2>&1; then
        timeout "$seconds" "$@"
    elif command -v gtimeout >/dev/null 2>&1; then
        gtimeout "$seconds" "$@"
    else
        if [ "$TIMEOUT_WARNED" -eq 0 ]; then
            echo -e "${YELLOW}[SETUP]${NC} timeout command not found; running without timeout safeguard."
            echo -e "${YELLOW}[SETUP]${NC} Install GNU coreutils for timeout: brew install coreutils"
            TIMEOUT_WARNED=1
        fi
        "$@"
    fi
}

wait_for_cassandra_writable() {
    local wait_seconds=0
    local wait_limit=180

    until docker exec cassandra1 cqlsh -e "CONSISTENCY LOCAL_ONE; CREATE TABLE IF NOT EXISTS ${CASSANDRA_KEYSPACE}.perf_healthcheck (id text PRIMARY KEY, ts timestamp); INSERT INTO ${CASSANDRA_KEYSPACE}.perf_healthcheck (id, ts) VALUES ('probe', toTimestamp(now())); SELECT ts FROM ${CASSANDRA_KEYSPACE}.perf_healthcheck WHERE id='probe'; DELETE FROM ${CASSANDRA_KEYSPACE}.perf_healthcheck WHERE id='probe';" >/dev/null 2>&1; do
        echo -e "${YELLOW}[INFRA]${NC} Waiting for Cassandra write readiness... (${wait_seconds}s)"
        sleep 2
        wait_seconds=$((wait_seconds + 2))

        if [ "$wait_seconds" -ge "$wait_limit" ]; then
            echo -e "${RED}✗ Cassandra is not writable within ${wait_limit}s${NC}"
            echo -e "${YELLOW}[INFRA]${NC} nodetool status from cassandra1:"
            docker exec cassandra1 nodetool status || true
            echo -e "${YELLOW}[INFRA]${NC} Recent cassandra1 logs:"
            docker logs --tail 50 cassandra1 || true
            exit 1
        fi
    done
}

# Parse arguments
SKIP_INFRA=false
ONLY_TESTS=""
for arg in "$@"; do
    case $arg in
        --skip-infra) SKIP_INFRA=true ;;
        --only=*) ONLY_TESTS="${arg#--only=}" ;;
    esac
done

echo -e "${GREEN}=====================================================================${NC}"
echo -e "${GREEN}Performance Test Suite: Stream Analytics Application${NC}"
echo -e "${GREEN}=====================================================================${NC}"

# Create results directory
mkdir -p "$RESULTS_DIR"
mkdir -p "$LOGS_DIR"

# Setup Java and Python environment
echo -e "\n${YELLOW}[SETUP]${NC} Configuring environment..."
export JAVA_HOME="$JAVA_HOME"
export PATH="$JAVA_HOME/bin:$PATH"

# Activate venv
if [ -f "$PROJECT_ROOT/.venv311/bin/activate" ]; then
    source "$PROJECT_ROOT/.venv311/bin/activate"
    echo -e "${GREEN}✓ Python venv activated${NC}"
else
    echo -e "${RED}✗ Python venv not found at $PROJECT_ROOT/.venv311${NC}"
    exit 1
fi

# Download java version if needed
java_version=$(java -version 2>&1 | head -1)
echo -e "${GREEN}✓ Java: $java_version${NC}"

# Start infrastructure if not skipping
if [ "$SKIP_INFRA" = false ]; then
    echo -e "\n${YELLOW}[INFRA]${NC} Starting Docker infrastructure..."

    docker compose down -v 2>/dev/null || true
    sleep 2

    docker compose up -d zookeeper-tenant2 kafka-tenant2 kafka-topic-init-tenant2 cassandra1 cassandra2 cassandra3 cassandra-init-tenant2

    echo -e "${YELLOW}[INFRA]${NC} Waiting for infrastructure to be ready..."
    sleep 15

    # Verify Kafka (cp-kafka image provides kafka-broker-api-versions, not .sh)
    kafka_wait_seconds=0
    kafka_wait_limit=180
    until docker exec kafka-tenant2 kafka-broker-api-versions --bootstrap-server kafka-tenant2:29092 >/dev/null 2>&1; do
        echo -e "${YELLOW}[INFRA]${NC} Waiting for Kafka... (${kafka_wait_seconds}s)"
        sleep 2
        kafka_wait_seconds=$((kafka_wait_seconds + 2))

        if [ "$kafka_wait_seconds" -ge "$kafka_wait_limit" ]; then
            echo -e "${RED}✗ Kafka did not become ready within ${kafka_wait_limit}s${NC}"
            echo -e "${YELLOW}[INFRA]${NC} Recent kafka-tenant2 logs:"
            docker logs --tail 50 kafka-tenant2 || true
            exit 1
        fi
    done
    echo -e "${GREEN}✓ Kafka ready${NC}"

    # Verify Cassandra
    until docker exec cassandra1 cqlsh -e "SELECT count(*) FROM system.peers;" >/dev/null 2>&1; do
        echo -e "${YELLOW}[INFRA]${NC} Waiting for Cassandra..."
        sleep 2
    done
    echo -e "${GREEN}✓ Cassandra ready${NC}"
else
    echo -e "${YELLOW}[INFRA]${NC} Skipping infrastructure startup${NC}"
fi

# Function to run a single test
run_test() {
    local test_name=$1
    local max_events=$2
    local emit_delay_ms=$3
    local window_config=$4  # Optional: override window function
    local producer_run_log="$LOGS_DIR/kafka_producer/${test_name}_producer.stdout.log"
    local flink_run_log="$LOGS_DIR/streamanalyticsapp/${test_name}_flink.stdout.log"
    local window_size_minutes=15
    local window_slide_seconds=60

    echo -e "\n${YELLOW}[TEST]${NC} Running: $test_name"
    echo -e "       Parameters: max_events=$max_events, emit_delay_ms=$emit_delay_ms"

    # Ensure Cassandra can satisfy LOCAL_ONE writes before test begins.
    wait_for_cassandra_writable

    # Clear Cassandra table for fresh results
    if ! docker exec cassandra1 cqlsh -e "TRUNCATE $CASSANDRA_KEYSPACE.stream_analytics_results;" >/dev/null 2>&1; then
        echo -e "${YELLOW}[TEST]${NC} Could not truncate table (it may not exist yet). Continuing..."
    fi
    sleep 2

    # Clear logs
    rm -f "$LOGS_DIR/kafka_producer/producer.log"
    rm -f "$LOGS_DIR/streamanalyticsapp/app.log"
    rm -f "$LOGS_DIR/streamanalyticsapp/metrics.log"
    rm -f "$LOGS_DIR/streamanalyticsapp/analytics_output.jsonl"
    rm -f "$producer_run_log" "$flink_run_log"
    mkdir -p "$LOGS_DIR/kafka_producer" "$LOGS_DIR/streamanalyticsapp"

    # Apply window config if provided
    if [ -n "$window_config" ]; then
        case "$window_config" in
            default)
                window_size_minutes=15
                window_slide_seconds=60
                ;;
            5m_30s)
                window_size_minutes=5
                window_slide_seconds=30
                ;;
            15m_1m)
                window_size_minutes=15
                window_slide_seconds=60
                ;;
            30m_5m)
                window_size_minutes=30
                window_slide_seconds=300
                ;;
            15m_10s)
                window_size_minutes=15
                window_slide_seconds=10
                ;;
            *)
                echo -e "${YELLOW}[TEST]${NC} Unknown window config '$window_config', using default 15m/60s"
                window_size_minutes=15
                window_slide_seconds=60
                ;;
        esac
        echo -e "${YELLOW}[TEST]${NC} Applying window config: ${window_size_minutes}m/${window_slide_seconds}s"
    fi

    local test_start=$(date +%s.%N)

    # Run producer
    echo -e "       ${YELLOW}[PRODUCER]${NC} Emitting $max_events events..."
    run_with_timeout 600 env \
        KAFKA_BROKERS="$KAFKA_BROKERS" \
        KAFKA_TOPIC="$KAFKA_TOPIC" \
        MAX_EVENTS="$max_events" \
        EMIT_DELAY_MS="$emit_delay_ms" \
        PRODUCER_LOG_DIR="$LOGS_DIR/kafka_producer" \
        python kafka_csv_producer_instrumented.py >"$producer_run_log" 2>&1 || true

    # Extract producer stats
    producer_stats=$(grep "SUMMARY" "$LOGS_DIR/kafka_producer/producer.log" 2>/dev/null | tail -1 || true)
    if [ -n "$producer_stats" ]; then
        echo -e "       ${GREEN}✓${NC} $producer_stats"
    else
        echo -e "       ${YELLOW}[PRODUCER]${NC} No SUMMARY line found; last output:"
        tail -n 20 "$producer_run_log" 2>/dev/null || true
    fi

    # Wait for producer to settle
    sleep 3

    # Run Flink analytics app
    echo -e "       ${YELLOW}[FLINK]${NC} Running analytics pipeline..."
    run_with_timeout 600 env \
        KAFKA_BROKERS="$KAFKA_BROKERS" \
        KAFKA_TOPIC="$KAFKA_TOPIC" \
        KAFKA_CONSUMER_GROUP="perf-test-$(date +%s)" \
        MAX_EVENTS="$max_events" \
        KAFKA_IDLE_TIMEOUT_MS="30000" \
        WINDOW_SIZE_MINUTES="$window_size_minutes" \
        WINDOW_SLIDE_SECONDS="$window_slide_seconds" \
        CASSANDRA_HOSTS="$CASSANDRA_HOSTS" \
        CASSANDRA_KEYSPACE="$CASSANDRA_KEYSPACE" \
        LOG_DIR="$LOGS_DIR/streamanalyticsapp" \
        python streamanalyticsapp_instrumented.py >"$flink_run_log" 2>&1 || true

    local test_end=$(date +%s.%N)
    local test_duration=$(echo "$test_end - $test_start" | bc)

    # Extract Flink stats
    flink_stats=$(grep "FINAL PERFORMANCE SUMMARY" "$LOGS_DIR/streamanalyticsapp/metrics.log" 2>/dev/null || true)
    if [ -n "$flink_stats" ]; then
        echo -e "       ${GREEN}✓${NC} Analytics pipeline completed"
    else
        echo -e "       ${YELLOW}[FLINK]${NC} FINAL PERFORMANCE SUMMARY not found; last output:"
        tail -n 40 "$flink_run_log" 2>/dev/null || true
    fi

    # Query Cassandra
    cassandra_count=$(docker exec cassandra1 cqlsh -e "SELECT count(*) FROM $CASSANDRA_KEYSPACE.stream_analytics_results;" 2>/dev/null | awk '/^[[:space:]]*[0-9]+[[:space:]]*$/{v=$1} END{if(v=="") v=0; print v}')
    echo -e "       ${GREEN}✓${NC} Cassandra rows: $cassandra_count"

    # Generate report
    echo -e "       ${YELLOW}[REPORT]${NC} Generating performance report..."
    python "$SCRIPT_DIR/analyze_performance.py" --test-name="$test_name" > /dev/null 2>&1

    echo -e "       ${GREEN}✓${NC} Test completed in ${test_duration}s"
    echo -e "       ${YELLOW}[LOGS]${NC} producer stdout: $producer_run_log"
    echo -e "       ${YELLOW}[LOGS]${NC} flink stdout: $flink_run_log"
}

# Define test scenarios (Bash 3 compatible; no associative arrays)
TEST_CASES=(
    "A1_burst|1000|0|default"
    "A2_moderate|1000|100|default"
    "A3_slow|600|1000|default"
    "B1_small_windows|1000|50|5m_30s"
    "B2_baseline_windows|1000|50|15m_1m"
    "B3_large_windows|1000|50|30m_5m"
    "B4_high_frequency|1000|50|15m_10s"
)

SELECTED_CASES=()

# Filter tests if --only specified
if [ -n "$ONLY_TESTS" ]; then
    IFS=',' read -ra requested_tests <<< "$ONLY_TESTS"

    for requested in "${requested_tests[@]}"; do
        requested=$(echo "$requested" | xargs)  # trim whitespace
        found=false

        for case_entry in "${TEST_CASES[@]}"; do
            IFS='|' read -r case_name _ <<< "$case_entry"
            if [ "$case_name" = "$requested" ]; then
                SELECTED_CASES+=("$case_entry")
                found=true
                break
            fi
        done

        if [ "$found" = false ]; then
            echo -e "${RED}✗ Unknown test: $requested${NC}"
        fi
    done
else
    SELECTED_CASES=("${TEST_CASES[@]}")
fi

# Run all selected tests
total_tests=${#SELECTED_CASES[@]}
if [ "$total_tests" -eq 0 ]; then
    echo -e "${RED}✗ No valid tests selected. Use --only=A1_burst,A2_moderate,...${NC}"
    exit 1
fi

completed=0

for case_entry in "${SELECTED_CASES[@]}"; do
    IFS='|' read -r test_name max_events emit_delay_ms window_config <<< "$case_entry"
    run_test "$test_name" "$max_events" "$emit_delay_ms" "$window_config"
    ((completed++))
    echo -e "       ${YELLOW}Progress: $completed/$total_tests${NC}"
done

# Generate comparison report
echo -e "\n${YELLOW}[REPORT]${NC} Generating comparison table..."
python "$SCRIPT_DIR/analyze_performance.py" | tail -20

echo -e "\n${GREEN}=====================================================================${NC}"
echo -e "${GREEN}Performance Test Suite Complete!${NC}"
echo -e "${GREEN}Results saved to: $RESULTS_DIR${NC}"
echo -e "${GREEN}=====================================================================${NC}"

# Print quick summary
echo -e "\n${YELLOW}Quick Summary:${NC}"
for metrics_file in "$RESULTS_DIR"/*_metrics.json; do
    if [ -f "$metrics_file" ]; then
        test_name=$(basename "$metrics_file" _metrics.json)
        echo ""
        echo -e "  ${YELLOW}$test_name:${NC}"
        python -c "
import json
with open('$metrics_file') as f:
    data = json.load(f)
    p = data['producer']
    f = data['flink']
    c = data['cassandra_row_count']
    print(f\"    Produced: {p['produced']} | Consumed: {f['kafka_consumed']} | Cassandra: {c}\")
    print(f\"    Prod Rate: {p['rate_events_sec']:.1f} ev/s | Cons Rate: {f['kafka_rate_events_sec']:.1f} ev/s\")
    print(f\"    Pipeline: {f['total_pipeline_time_sec']:.1f}s | Amplification: {f['amplification_factor']:.1f}x\")
" 2>/dev/null || true
    fi
done
