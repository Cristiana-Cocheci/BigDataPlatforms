# Performance Testing Checklist & Execution Guide

## Pre-Test Setup

- [ ] Update code directory: `cd assignment-3-103803829/code`
- [ ] Activate Python environment: `source .venv311/bin/activate`
- [ ] Verify Java: `java -version` (should be OpenJDK 11+)
- [ ] Set JAVA_HOME: `export JAVA_HOME=/opt/homebrew/opt/openjdk@11/libexec/openjdk.jdk/Contents/Home`
- [ ] Create results directory: `mkdir -p results`
- [ ] Clear previous logs: `rm -rf logs/`

## Baseline Measurement (Current Code)

Run this first to establish baseline with existing code:

```bash
# 1. Start infrastructure
docker compose down -v
docker compose up -d zookeeper-tenant2 kafka-tenant2 kafka-topic-init-tenant2 cassandra1 cassandra2 cassandra3 cassandra-init-tenant2
sleep 15

# 2. Run producer (current version)
KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=400 EMIT_DELAY_MS=0 python kafka_csv_producer.py

# 3. Run Flink app (current version)
time KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=baseline-$(date +%s) MAX_EVENTS=400 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 python streamanalyticsapp.py

# 4. Check Cassandra
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"

# 5. Save logs
cp logs/kafka_producer/producer.log results/baseline_producer.log.txt
cp logs/streamanalyticsapp/app.log results/baseline_flink.log.txt
wc -l logs/streamanalyticsapp/analytics_output.jsonl > results/baseline_output_count.txt
```

**Baseline Checks**:
- [ ] Producer completes successfully
- [ ] Cassandra receives rows
- [ ] No fatal errors in logs
- [ ] Note: No detailed timing metrics available in baseline

---

## Test Suite: Scenario A (Streaming Speed Variation)

### A1: Burst Mode

```bash
# Clear and restart
docker exec cassandra1 cqlsh -e "TRUNCATE mysimbdp_tenant2.stream_analytics_results;"
rm -rf logs/

# Run instrumented versions
echo "=== A1: BURST MODE ===" >> results/test_log.txt
date >> results/test_log.txt

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=1000 EMIT_DELAY_MS=0 PRODUCER_LOG_DIR=logs/kafka_producer \
  python kafka_csv_producer_instrumented.py

sleep 2

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=a1-burst-$(date +%s) MAX_EVENTS=1000 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 LOG_DIR=logs/streamanalyticsapp \
  python streamanalyticsapp_instrumented.py

# Generate report
python auxx/analyze_performance.py --test-name="A1_burst"

# Verify results
echo "Producer log:"
tail -1 logs/kafka_producer/producer.log
echo "Flink metrics:"
grep "FINAL PERFORMANCE SUMMARY" -A 15 logs/streamanalyticsapp/metrics.log
echo "Cassandra:"
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"
```

**A1 Checklist**:
- [ ] Producer produces 1000 events in < 5 seconds
- [ ] Production rate > 100 events/sec
- [ ] Cassandra writes > 6000 rows
- [ ] No data loss (produced == consumed)
- [ ] Report generated in results/A1_burst_report.txt

### A2: Moderate Speed (Realistic)

```bash
docker exec cassandra1 cqlsh -e "TRUNCATE mysimbdp_tenant2.stream_analytics_results;"
rm -rf logs/

echo "=== A2: MODERATE SPEED ===" >> results/test_log.txt
date >> results/test_log.txt

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=1000 EMIT_DELAY_MS=100 PRODUCER_LOG_DIR=logs/kafka_producer \
  python kafka_csv_producer_instrumented.py

sleep 2

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=a2-moderate-$(date +%s) MAX_EVENTS=1000 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 LOG_DIR=logs/streamanalyticsapp \
  python streamanalyticsapp_instrumented.py

python auxx/analyze_performance.py --test-name="A2_moderate"

echo "Producer rate:"
grep "rate_events_sec=" logs/kafka_producer/producer.log | tail -1
echo "Flink consumed:"
grep "kafka_consumed=" logs/streamanalyticsapp/metrics.log | tail -1
echo "Cassandra:"
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"
```

**A2 Checklist**:
- [ ] Producer produces ~10 events/sec (100ms delay)
- [ ] Production time ~100 seconds
- [ ] Consumption rate > 50 events/sec
- [ ] Cassandra reads ~6000-7000 rows
- [ ] Report saved

### A3: Slow Streaming

```bash
docker exec cassandra1 cqlsh -e "TRUNCATE mysimbdp_tenant2.stream_analytics_results;"
rm -rf logs/

echo "=== A3: SLOW STREAMING ===" >> results/test_log.txt
date >> results/test_log.txt

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=600 EMIT_DELAY_MS=1000 PRODUCER_LOG_DIR=logs/kafka_producer \
  python kafka_csv_producer_instrumented.py

sleep 2

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=a3-slow-$(date +%s) MAX_EVENTS=600 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 LOG_DIR=logs/streamanalyticsapp \
  python streamanalyticsapp_instrumented.py

python auxx/analyze_performance.py --test-name="A3_slow"

echo "Producer rate:"
grep "rate_events_sec=" logs/kafka_producer/producer.log | tail -1
echo "Cassandra:"
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"
```

**A3 Checklist**:
- [ ] Producer rate ~1 event/sec
- [ ] Production time ~600 seconds (10 min)
- [ ] No timeouts
- [ ] Cassandra writes ~4000-4500 rows
- [ ] Report saved

---

## Test Suite: Scenario B (Window Parameter Variation)

⚠️ **NOTE**: For B1, B3, B4 tests, you need to manually edit [streamanalyticsapp_instrumented.py line 427](streamanalyticsapp_instrumented.py#L427) before running.

### B1: Small Windows (5-min / 30-sec slide)

**Before running, edit streamanalyticsapp_instrumented.py**:

Find line 427:
```python
.window(SlidingEventTimeWindows.of(Time.minutes(15), Time.minutes(1)))
```

Replace with:
```python
.window(SlidingEventTimeWindows.of(Time.minutes(5), Time.seconds(30)))
```

Then run:
```bash
docker exec cassandra1 cqlsh -e "TRUNCATE mysimbdp_tenant2.stream_analytics_results;"
rm -rf logs/

echo "=== B1: SMALL WINDOWS (5m/30s) ===" >> results/test_log.txt
date >> results/test_log.txt

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=1000 EMIT_DELAY_MS=50 PRODUCER_LOG_DIR=logs/kafka_producer \
  python kafka_csv_producer_instrumented.py

sleep 2

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=b1-small-$(date +%s) MAX_EVENTS=1000 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 LOG_DIR=logs/streamanalyticsapp \
  python streamanalyticsapp_instrumented.py

python auxx/analyze_performance.py --test-name="B1_small_windows"

echo "Amplification:"
grep "amplification_factor=" logs/streamanalyticsapp/metrics.log | tail -1
echo "Cassandra:"
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"
```

**B1 Checklist**:
- [ ] Windows emitted >> 6000 (more frequent windows)
- [ ] Cassandra rows >> 10000 (expected ~15000+)
- [ ] Amplification factor ~15-20x
- [ ] Avg write latency < 2ms (or GC spikes)
- [ ] Report saved

### B2: Baseline Windows (15-min / 1-min slide)

**Reset to default** (line 427):
```python
.window(SlidingEventTimeWindows.of(Time.minutes(15), Time.minutes(1)))
```

Run:
```bash
docker exec cassandra1 cqlsh -e "TRUNCATE mysimbdp_tenant2.stream_analytics_results;"
rm -rf logs/

echo "=== B2: BASELINE WINDOWS (15m/1m) ===" >> results/test_log.txt
date >> results/test_log.txt

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=1000 EMIT_DELAY_MS=50 PRODUCER_LOG_DIR=logs/kafka_producer \
  python kafka_csv_producer_instrumented.py

sleep 2

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=b2-baseline-$(date +%s) MAX_EVENTS=1000 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 LOG_DIR=logs/streamanalyticsapp \
  python streamanalyticsapp_instrumented.py

python auxx/analyze_performance.py --test-name="B2_baseline_windows"

echo "Baseline comparison:"
grep "windows_emitted=" logs/streamanalyticsapp/metrics.log | tail -1
echo "Cassandra:"
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"
```

**B2 Checklist**:
- [ ] Windows ~6000-7000
- [ ] Cassandra rows ~6000-7000
- [ ] Amplification ~6-7x (as per original baseline)
- [ ] Write latency <2ms
- [ ] Report saved

### B3: Large Windows (30-min / 5-min slide)

**Edit line 427**:
```python
.window(SlidingEventTimeWindows.of(Time.minutes(30), Time.minutes(5)))
```

Run:
```bash
docker exec cassandra1 cqlsh -e "TRUNCATE mysimbdp_tenant2.stream_analytics_results;"
rm -rf logs/

echo "=== B3: LARGE WINDOWS (30m/5m) ===" >> results/test_log.txt
date >> results/test_log.txt

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=1000 EMIT_DELAY_MS=50 PRODUCER_LOG_DIR=logs/kafka_producer \
  python kafka_csv_producer_instrumented.py

sleep 2

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=b3-large-$(date +%s) MAX_EVENTS=1000 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 LOG_DIR=logs/streamanalyticsapp \
  python streamanalyticsapp_instrumented.py

python auxx/analyze_performance.py --test-name="B3_large_windows"

echo "Large windows comparison:"
grep "windows_emitted=" logs/streamanalyticsapp/metrics.log | tail -1
echo "Cassandra:"
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"
```

**B3 Checklist**:
- [ ] Windows << 6000 (fewer, larger windows)
- [ ] Cassandra rows ~3000-4000
- [ ] Amplification ~3-4x (lowest)
- [ ] Write latency <2ms
- [ ] Report saved

### B4: High-Frequency Slides (15-min / 10-sec slide)

⚠️ **WARNING**: This creates ~90 overlapping windows per event. Stress test!

**Edit line 427**:
```python
.window(SlidingEventTimeWindows.of(Time.minutes(15), Time.seconds(10)))
```

Run:
```bash
docker exec cassandra1 cqlsh -e "TRUNCATE mysimbdp_tenant2.stream_analytics_results;"
rm -rf logs/

echo "=== B4: HIGH FREQUENCY (15m/10s) ===" >> results/test_log.txt
date >> results/test_log.txt

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=500 EMIT_DELAY_MS=50 PRODUCER_LOG_DIR=logs/kafka_producer \
  python kafka_csv_producer_instrumented.py

sleep 2

time \
  KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=b4-highfreq-$(date +%s) MAX_EVENTS=500 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 LOG_DIR=logs/streamanalyticsapp \
  python streamanalyticsapp_instrumented.py

python auxx/analyze_performance.py --test-name="B4_high_frequency"

echo "High frequency comparison:"
grep "windows_emitted=" logs/streamanalyticsapp/metrics.log | tail -1
echo "Cassandra:"
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"
```

**B4 Checklist**:
- [ ] Windows MUCH higher (40000+)
- [ ] Cassandra rows 40000+
- [ ] Amplification ~80-90x (extreme)
- [ ] Max write latency may show spikes
- [ ] Some write errors possible (expected)
- [ ] Report saved

---

## Generating Comparison Report

After all tests complete:

```bash
# Generate comparison table
python auxx/analyze_performance.py

# View all reports
ls -lh results/

# Compare specific tests
echo "=== A1 vs A2 vs A3 ==="
for test in A1_burst A2_moderate A3_slow; do
  cat results/${test}_metrics.json | python -m json.tool
done

# Export to CSV
python -c "
import json
import csv
from pathlib import Path

data = []
for f in sorted(Path('results').glob('*_metrics.json')):
    with open(f) as fp:
        m = json.load(fp)
        data.append({
            'test': m['test_name'],
            'produced': m['producer']['produced'],
            'consumed': m['flink']['kafka_consumed'],
            'cassandra': m['cassandra_row_count'],
            'prod_rate': m['producer']['rate_events_sec'],
            'cons_rate': m['flink']['kafka_rate_events_sec'],
            'avg_write_ms': m['flink']['cassandra_avg_write_ms'],
            'amplification': m['flink']['amplification_factor'],
            'total_time': m['flink']['total_pipeline_time_sec'],
        })

with open('results/comparison.csv', 'w') as f:
    w = csv.DictWriter(f, data[0].keys())
    w.writeheader()
    w.writerows(data)

print(f'Saved {len(data)} test results to results/comparison.csv')
"

cat results/comparison.csv
```

---

## Cleanup

```bash
# Stop infrastructure
docker compose down -v

# Archive results
tar -czf results_$(date +%Y%m%d_%H%M%S).tar.gz results/ logs/

# View final metrics
cat results/comparison.csv
```

---

## Expected Results Summary

| Test | Events | Rate | Cassandra | Amplif | Notes |
|------|--------|------|-----------|--------|-------|
| A1 | 1000 | 200+/s | 6700 | 6.7x | Burst mode baseline |
| A2 | 1000 | 9.7/s | 6700 | 6.75x | Realistic streaming |
| A3 | 600 | 1/s | 4000 | 6.7x | Low stress |
| B1 | 1000 | 9.7/s | 15000+ | 15x | Small windows (heavy) |
| B2 | 1000 | 9.7/s | 6700 | 6.75x | Baseline |
| B3 | 1000 | 9.7/s | 3500 | 3.5x | Large windows (light) |
| B4 | 500 | 9.7/s | 45000+ | 90x | Extreme (stress test) |

---

## Troubleshooting

### Cassandra errors
```bash
# Check if running
docker ps | grep cassandra

# Check keyspace
docker exec cassandra1 cqlsh -e "DESCRIBE KEYSPACES;"

# Restart cassandra
docker compose restart cassandra1 cassandra2 cassandra3
```

### Kafka no events
```bash
# Check topic
docker exec kafka-tenant2 kafka-topics --list --bootstrap-server kafka-tenant2:29092

# Check message count
docker exec kafka-tenant2 kafka-consumer-groups --bootstrap-server kafka-tenant2:29092 --group a1-burst-123 --describe

# Verify producer
grep "SUMMARY" logs/kafka_producer/producer.log
```

### Flink can't connect
```bash
# Check Java
java -version
export JAVA_HOME=/opt/homebrew/opt/openjdk@11/libexec/openjdk.jdk/Contents/Home

# Borrow PyFlink test
python -c "from pyflink.datastream import StreamExecutionEnvironment; print('OK')"
```

### No metrics logged
```bash
# Ensure using instrumented versions
grep "METRICS\|SUMMARY\|FINAL PERFORMANCE" logs/streamanalyticsapp/metrics.log

# Check file permissions
ls -la logs/streamanalyticsapp/
```

---

## Success Criteria

✅ All tests pass if:
- [ ] A1, A2, A3 complete without errors
- [ ] B2 baseline produces expected amplification
- [ ] All comparison reports generated
- [ ] Zero data loss in all scenarios
- [ ] Cassandra row counts match expected amplification
- [ ] No fatal errors in logs

🎯 Performance optimization ready if:
- [ ] Metrics collected for all stages
- [ ] Trade-offs quantified (speed vs write latency)
- [ ] Bottleneks identified
- [ ] Baseline established for production tuning
