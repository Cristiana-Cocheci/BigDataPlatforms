# Performance Testing Plan: Stream Analytics Application

## 1. Current State of Performance Metrics

### Current Observations Available
From the existing codebase:
- **Kafka Producer** ([kafka_csv_producer.py](kafka_csv_producer.py#L68-L117)):
  - Logs `produced` count (successfully published events)
  - Logs `skipped` count (malformed rows)
  - No timing measurements
  
- **Flink Analytics** ([streamanalyticsapp.py](streamanalyticsapp.py#L360-L438)):
  - Logs startup config and environment variables
  - Logs `analytics_output.jsonl` with one JSON per processed window
  - No explicit throughput or latency tracking
  
- **Cassandra Sink** ([streamanalyticsapp.py](streamanalyticsapp.py#L149-L255)):
  - Logs write failures but not success count
  - No metrics on insert latency

### Validated Baseline Run
- **Input**: 400 CSV records
- **Kafka Published**: 400 events
- **Flink Consumed**: 400 events (sliding 15-min windows, 1-min slide)
- **Cassandra Inserted**: 5985 rows (400 events × multiple windows per sensor)
- **No timing data collected in baseline run**

---

## 2. Performance Metrics to Collect

### 2.1 Producer Metrics
| Metric | Unit | How to Measure | Value Source |
|--------|------|----------------|--------------|
| **Total Events Produced** | count | Parse producer.log `produced=` | Kafka producer stdout |
| **Total Events Skipped** | count | Parse producer.log `skipped=` | Kafka producer stdout |
| **Production Time** | seconds | Wall-clock start to finish | timestamp diff in logs |
| **Production Rate** | events/sec | Total Events / Production Time | Calculated |
| **Average Emit Delay** | ms | EMIT_DELAY_MS environment variable | Test configuration |

### 2.2 Kafka Broker Metrics
| Metric | Unit | How to Measure | Value Source |
|--------|------|----------------|--------------|
| **Messages in Topic** | count | Kafka consumer group offset | `docker exec kafka-tenant2 kafka-consumer-groups` |
| **Topic Lag** | count | Committed offset vs latest offset | Kafka metrics endpoint |
| **Broker Throughput** | MB/sec | Message size × rate | Kafka logs |

### 2.3 Flink Consumer Metrics
| Metric | Unit | How to Measure | Value Source |
|--------|------|----------------|--------------|
| **Events Consumed from Kafka** | count | Parse streamanalyticsapp.log | Events successfully parsed |
| **Consumption Time** | seconds | `load_events_from_kafka()` wall-clock time | Instrumented in code |
| **Average Consumption Latency** | ms | Event ingestion to window output | Timestamp diff (event_ts → processed_ts) |
| **Windows Emitted** | count | Lines in analytics_output.jsonl | Count output records |
| **Window Processing Rate** | windows/sec | Total Windows / Total Time | Calculated |

### 2.4 Cassandra Sink Metrics
| Metric | Unit | How to Measure | Value Source |
|--------|------|----------------|--------------|
| **Rows Inserted to Cassandra** | count | `SELECT count(*) FROM stream_analytics_results` | Cassandra query |
| **Insertion Time** | seconds | Cassandra write start to finish | Instrumented code |
| **Insert Latency** | ms | Per-row write time | Batch insert latency |
| **Insert Rate** | rows/sec | Total Rows / Insertion Time | Calculated |
| **Cassandra Write Errors** | count | Parse app.log for "cassandra write failed" | Error logs |

### 2.5 End-to-End System Metrics
| Metric | Unit | How to Measure | Value Source |
|--------|------|----------------|--------------|
| **Total Pipeline Time** | seconds | Producer start → Cassandra complete | Master timer |
| **Pipeline Throughput** | events/sec | Input Events / Pipeline Time | Calculated |
| **Amplification Factor** | ratio | Cassandra Rows / Input Events | 5985 / 400 = 14.96x |
| **Data Loss** | % | (Produced - Consumed) / Produced × 100 | 0 if no backpressure |

---

## 3. Testing Scenarios

### Scenario A: Varying Streaming Speed (Kafka Producer Speed)

#### A1: No Delay (Burst Mode)
- **Configuration**:
  - `EMIT_DELAY_MS=0` (immediate emission)
  - `MAX_EVENTS=1000`
  - Window: 15-min / 1-min slide
  - Cassandra: 3-node cluster
  
- **Expected Behavior**:
  - Producer publishes all 1000 events as fast as network allows
  - Flink consumer receives burst of events
  - Potential: events arriving faster than window boundaries
  
- **Metrics to Compare**:
  - Production time: < 5 seconds (no latency between events)
  - Consumption rate: High (many events per second)
  - Cassandra write errors: Possibly higher due to concurrency

#### A2: Moderate Delay (Realistic Streaming)
- **Configuration**:
  - `EMIT_DELAY_MS=100` (100ms between events)
  - `MAX_EVENTS=1000`
  - Same window parameters
  
- **Expected Behavior**:
  - Events arrive at ~10 events/second
  - More realistic sensor data arrival pattern
  - Better window boundary distribution
  
- **Metrics to Compare**:
  - Production time: ~100 seconds (100ms × 1000 events)
  - Consumption rate: Steady and lower than A1
  - Cassandra writes: More stable, fewer conflicts

#### A3: High Delay (Slow Streaming)
- **Configuration**:
  - `EMIT_DELAY_MS=1000` (1 second between events)
  - `MAX_EVENTS=600`
  - Same window parameters
  
- **Expected Behavior**:
  - Events trickle in slowly (1 per second)
  - Flink has time to process between arrivals
  - Windows may have fewer events per interval
  
- **Metrics to Compare**:
  - Production time: ~600 seconds (10 minutes)
  - Consumption rate: ~1 event/second
  - Cassandra latency: Per-event latency very low
  - Records per window: Likely fewer than burst mode

---

### Scenario B: Varying Window Function Parameters

#### B1: Small Windows (5-min / 30-sec slide)
- **Configuration**:
  - Modify [streamanalyticsapp.py line 427](streamanalyticsapp.py#L427):
    ```python
    .window(SlidingEventTimeWindows.of(Time.minutes(5), Time.seconds(30)))
    ```
  - `MAX_EVENTS=1000`
  - `EMIT_DELAY_MS=50`
  
- **Expected Behavior**:
  - More windows per event (5-min window / 30-sec slide = ~10 overlapping windows)
  - Higher amplification factor
  - More Cassandra rows
  
- **Metrics**:
  - Windows emitted: Much higher (>15,000 rows for 1000 events)
  - Amplification: ~15x to 20x
  - Processing time: Higher due to more windows
  - Memory usage: More state to track

#### B2: Default Windows (15-min / 1-min slide)
- **Configuration**:
  - Current [line 427](streamanalyticsapp.py#L427): `SlidingEventTimeWindows.of(Time.minutes(15), Time.minutes(1))`
  - `MAX_EVENTS=1000`
  - `EMIT_DELAY_MS=50`
  
- **Baseline** for comparison
- **Expected Metrics**:
  - Windows emitted: ~6,000-7,000 rows for 1000 events
  - Amplification: ~6-7x
  - Latency: Higher but aggregates more data per window

#### B3: Large Windows (30-min / 5-min slide)
- **Configuration**:
  - Modify [line 427](streamanalyticsapp.py#L427):
    ```python
    .window(SlidingEventTimeWindows.of(Time.minutes(30), Time.minutes(5)))
    ```
  - `MAX_EVENTS=1000`
  - `EMIT_DELAY_MS=50`
  
- **Expected Behavior**:
  - Fewer windows per event (30-min window / 5-min slide = 6 overlapping windows)
  - Lower amplification factor
  - Fewer Cassandra rows
  
- **Metrics**:
  - Windows emitted: Lower (~3,000-4,000 rows for 1000 events)
  - Amplification: ~3-4x
  - Processing time: Lower
  - Better aggregation of long-term trends

#### B4: High-Frequency Slides (15-min / 10-sec slide)
- **Configuration**:
  - Modify [line 427](streamanalyticsapp.py#L427):
    ```python
    .window(SlidingEventTimeWindows.of(Time.minutes(15), Time.seconds(10)))
    ```
  - `MAX_EVENTS=1000`
  - `EMIT_DELAY_MS=50`
  
- **Expected Behavior**:
  - Maximum amplification (15-min window / 10-sec slide = 90 overlapping windows!)
  - Very high Cassandra write load
  - Maximum near-real-time responsiveness
  
- **Metrics**:
  - Windows emitted: Very high (~50,000-60,000 rows for 1000 events)
  - Amplification: ~50-60x
  - Cassandra write latency: Critical test point
  - Cassandra errors: Likely to appear due to load

---

## 4. Performance Measurement Infrastructure

### 4.1 Enhanced Producer Timing
**Modify** [kafka_csv_producer.py](kafka_csv_producer.py) to capture:
```python
import time

start_time = time.time()
# ... produce events ...
elapsed = time.time() - start_time
production_rate = produced / elapsed if elapsed > 0 else 0
log_line(log_path, f"SUMMARY produced={produced} skipped={skipped} elapsed_sec={elapsed:.2f} rate_events_sec={production_rate:.1f}")
```

### 4.2 Enhanced Kafka Consumer Timing
**Modify** [streamanalyticsapp.py line 291-342](streamanalyticsapp.py#L291-L342) (`load_events_from_kafka`):
```python
def load_events_from_kafka(...):
    start_time = time.time()
    events = []
    # ... poll loop ...
    elapsed = time.time() - start_time
    consumption_rate = len(events) / elapsed if elapsed > 0 else 0
    log_info(f"KAFKA_METRICS consumed={len(events)} elapsed_sec={elapsed:.2f} rate_events_sec={consumption_rate:.1f}")
    return events
```

### 4.3 Cassandra Write Timing
**Modify** [streamanalyticsapp.py line 149-255](streamanalyticsapp.py#L149-L255) (`CallbackMapFunction.map`):
```python
def map(self, value):
    write_start = time.time()
    # ... cassandra write ...
    write_elapsed = time.time() - write_start
    # Log per-write timing (sample every Nth record to avoid log spam)
```

### 4.4 Window Function Metrics
**Modify** [streamanalyticsapp.py line 79-147](streamanalyticsapp.py#L79-L147) (`AnalyticsWindowFunction.process`):
```python
def process(self, key, context, elements):
    records = list(elements)
    window_records = len(records)
    window_start_ts = context.window().start / 1000  # Convert milliseconds to seconds
    # Log window size and records count
    log_info(f"WINDOW_METRICS sensor_id={key} window_start={window_start_ts} records_in_window={window_records}")
```

### 4.5 Result Aggregation Script
**Create** [auxx/analyze_performance.py](auxx/analyze_performance.py):
```bash
#!/usr/bin/env python3
"""
Parses log files from producer, flink, and cassandra to generate performance summary.
Usage: python auxx/analyze_performance.py
"""
import re
from pathlib import Path
from collections import defaultdict

def parse_producer_log(log_path):
    with open(log_path) as f:
        lines = f.readlines()
    
    summary = {}
    for line in lines:
        if "SUMMARY" in line:
            match = re.search(r'produced=(\d+).*skipped=(\d+).*elapsed_sec=([\d.]+).*rate_events_sec=([\d.]+)', line)
            if match:
                summary['produced'] = int(match.group(1))
                summary['skipped'] = int(match.group(2))
                summary['elapsed_sec'] = float(match.group(3))
                summary['rate_events_sec'] = float(match.group(4))
    return summary

def parse_flink_log(log_path):
    metrics = {
        'consumed': 0,
        'windows_emitted': 0,
        'window_sizes': [],
        'cassandra_write_errors': 0
    }
    with open(log_path) as f:
        for line in f:
            if "KAFKA_METRICS" in line:
                match = re.search(r'consumed=(\d+).*elapsed_sec=([\d.]+)', line)
                if match:
                    metrics['consumed'] = int(match.group(1))
            elif "CASSANDRA write failed" in line:
                metrics['cassandra_write_errors'] += 1
    return metrics

def query_cassandra_rows():
    """Query actual cassandra row count"""
    import subprocess
    result = subprocess.run(
        ['docker', 'exec', 'cassandra1', 'cqlsh', '-e', 
         'SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;'],
        capture_output=True, text=True
    )
    lines = result.stdout.strip().split('\n')
    for line in lines:
        if line.strip().isdigit():
            return int(line.strip())
    return 0

# Generate report
print("=" * 80)
print("PERFORMANCE TEST REPORT")
print("=" * 80)
```

---

## 5. Testing Execution Plan

### Test Execution Order

**Phase 1: Baseline Measurement** (Current State)
```bash
cd assignment-3-103803829/code
source .venv311/bin/activate
export JAVA_HOME=/opt/homebrew/opt/openjdk@11/libexec/openjdk.jdk/Contents/Home
export PATH="$JAVA_HOME/bin:$PATH"

# Start infrastructure
docker compose up -d zookeeper-tenant2 kafka-tenant2 kafka-topic-init-tenant2 cassandra1 cassandra2 cassandra3 cassandra-init-tenant2

# Test A2 (Moderate Delay, Baseline Window)
KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements MAX_EVENTS=400 EMIT_DELAY_MS=100 python kafka_csv_producer.py

KAFKA_BROKERS=127.0.0.1:9094 KAFKA_TOPIC=dht22-measurements KAFKA_CONSUMER_GROUP=baseline-$(date +%s) MAX_EVENTS=400 KAFKA_IDLE_TIMEOUT_MS=30000 CASSANDRA_HOSTS=127.0.0.1 CASSANDRA_KEYSPACE=mysimbdp_tenant2 python streamanalyticsapp.py

# Check results
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"

# Collect logs
cp logs/kafka_producer/producer.log results/baseline_producer.log
cp logs/streamanalyticsapp/app.log results/baseline_flink.log
cp logs/streamanalyticsapp/analytics_output.jsonl results/baseline_analytics.jsonl
```

**Phase 2: Streaming Speed Tests (Scenario A)**
- Run A1, A2, A3 in sequence with fresh Cassandra table for each
- Capture all logs and cassandra row counts
- Calculate production rate, consumption rate, total time

**Phase 3: Window Parameter Tests (Scenario B)**
- Modify [line 427](streamanalyticsapp.py#L427) for each window config
- Run B1, B2, B3, B4 with consistent EMIT_DELAY_MS=50
- Track window count, amplification factor, insert latency

---

## 6. Performance Comparison Matrix

| Test Case | Producer Time | Production Rate | Events Consumed | Windows Emitted | Cassandra Rows | Amplification | Notes |
|-----------|---------------|-----------------|-----------------|-----------------|-----------------|---------------|-------|
| A1 (Burst) | < 5s | > 200 events/sec | ? | ? | ? | ? | High throughput test |
| A2 (Moderate) | ~100s | ~10 events/sec | ? | ? | ? | 6-7x | Baseline realistic |
| A3 (Slow) | ~600s | ~1 event/sec | ? | ? | ? | ? | Low stress test |
| B1 (5m/30s) | ~100s | ~10 events/sec | ? | ? | 15k+ | 15-20x | Max windows |
| B2 (15m/1m) | ~100s | ~10 events/sec | ? | ~6-7k | ~6-7k | 6-7x | Baseline window |
| B3 (30m/5m) | ~100s | ~10 events/sec | ? | ~3-4k | ~3-4k | 3-4x | Min windows |
| B4 (15m/10s) | ~100s | ~10 events/sec | ? | 50-60k | 50-60k | 50-60x | Extreme load |

---

## 7. Expected Findings & Hypotheses

### H1: Production Speed vs Consumption
- **Hypothesis**: Flink consumer can keep up with even burst mode (A1)
- **Expected Result**: Consumption rate matches production within bounded Kafka read timeout
- **Validation**: Check KAFKA_IDLE_TIMEOUT_MS behavior; if timeout fires too early, consumer misses events

### H2: Window Size Impact on Latency
- **Hypothesis**: Larger windows (B3) = lower per-window compute cost; smaller windows (B1) = higher state memory
- **Expected Result**: B3 has lowest latency, B1 highest; memory usage inversely correlated
- **Validation**: Monitor Flink process memory and window processing times per scenario

### H3: Cassandra Write Bottleneck
- **Hypothesis**: High-frequency windows (B4) will cause Cassandra write errors or latency spikes
- **Expected Result**: B4 shows measurable write failures or timeouts
- **Validation**: Count errors in app.log; measure per-write latency trends over time

### H4: Amplification Factor Linearity
- **Hypothesis**: Amplification = Window Duration / Slide Interval (constant for fixed event count)
- **Expected Result**: B1 ≈ 15-min/30-sec = 30x; B2 ≈ 15-min/1-min = 15x; B3 ≈ 30-min/5-min = 6x
- **Validation**: Count output rows and compute actual ratio

---

## 8. Constraints & Assumptions

1. **Single Parallelism**: [line 393](streamanalyticsapp.py#L393) sets `env.set_parallelism(1)` → single-threaded Flink
   - Implication: Flink won't parallelize window computation across cores
   - Mitigation: Results represent conservative baseline; production would use higher parallelism

2. **Bounded Kafka Read**: MAX_EVENTS limit means tests won't produce infinite streams
   - Beneficial for controlled testing but not representative of production streaming

3. **Cassandra Replication Factor = 1**: Single node writes
   - Real clusters would have RF=3 and higher write latency
   - Results are lower-bound latency

4. **Event Time Only**: Uses event timestamp from CSV, not processing time
   - Watermark strategy: bounded out-of-orderness (default 3 minutes)
   - Late arrivals will be dropped → real data would need handling

---

## 9. Report Sections

Final performance report should include:

1. **Executive Summary** 
   - Peak throughput achieved (events/sec)
   - Optimal window configuration for use case
   - Bottleneck identification

2. **Detailed Results per Scenario**
   - Producer timing and rates
   - Kafka topic metrics
   - Flink consumption and window processing
   - Cassandra insert latency and errors

3. **Trade-off Analysis**
   - Throughput vs latency
   - Amplification vs responsiveness
   - Streaming speed impact on stability

4. **Recommendations**
   - Suggested window parameters for production
   - Parallelism settings
   - Resource allocation (Cassandra nodes, Flink parallelism)

---

## 10. Log Parsing Examples

### Extract Producer Metrics
```bash
grep "SUMMARY" logs/kafka_producer/producer.log | tail -5
```

### Extract Cassandra Row Count
```bash
docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results ALLOW FILTERING;"
```

### Count Windows Emitted
```bash
wc -l logs/streamanalyticsapp/analytics_output.jsonl
```

### Extract Error Count
```bash
grep "cassandra write failed\|callback failed" logs/streamanalyticsapp/app.log | wc -l
```

### Sample Analytics Output
```bash
head -3 logs/streamanalyticsapp/analytics_output.jsonl | python -m json.tool
```

---

## 11. Next Steps

1. **Instrument producer** with timing metrics
2. **Instrument Flink consumer** with consumption timing
3. **Instrument window function** with per-window metrics
4. **Create shell script** to run test suite (all scenarios A1-A3, B1-B4)
5. **Generate performance report** with comparative tables
6. **Analyze bottlenecks** and recommend optimization
