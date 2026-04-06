# Performance Metrics Summary - Stream Analytics

## Current vs New Capabilities

### BEFORE (Current Code)

| Component | Metrics Available |
|-----------|------------------|
| **Producer** (`kafka_csv_producer.py`) | ✓ Event count, Skip count<br>✗ Production time<br>✗ Throughput |
| **Flink App** (`streamanalyticsapp.py`) | ✓ Config logging<br>✓ Error logging<br>✗ Consumption timing<br>✗ Window metrics<br>✗ Write latency |
| **Cassandra** | ✗ No instrumentation<br>✗ Manual query needed |

**Summary**: ~20% of needed metrics; mostly reactive (error logging only)

---

### AFTER (New Instrumented Versions)

| Component | Metrics Available |
|-----------|------------------|
| **Producer** (`kafka_csv_producer_instrumented.py`) | ✓ Event count, Skip count<br>✓ **Production time**<br>✓ **Production rate** (events/sec)<br>✓ **Emit lag** (first-to-last event)<br>✓ **Average emit delay** (actual vs configured) |
| **Flink App** (`streamanalyticsapp_instrumented.py`) | ✓ Config logging<br>✓ Error logging<br>✓ **Kafka consumption time**<br>✓ **Consumption rate** (events/sec)<br>✓ **Window size metrics**<br>✓ **Cassandra write latency** (per-batch and aggregate)<br>✓ **Amplification factor**<br>✓ **Total pipeline time**<br>✓ **Data loss percentage** |
| **Analysis Tools** (`analyze_performance.py`) | ✓ **Automatic report generation**<br>✓ **Comparison tables**<br>✓ **JSON export for further analysis** |

**Summary**: ~95% of needed metrics; proactive instrumentation at key points

---

## Quick Reference: Metrics Flow

```
┌─────────────────────────────────────────────────────────────────┐
│                    INPUT EVENTS (CSV)                           │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│  PRODUCER (kafka_csv_producer_instrumented.py)                  │
│  ✓ produced=1000                                                 │
│  ✓ elapsed_sec=41.24                                             │
│  ✓ rate_events_sec=24.27                                         │
│  ✓ avg_emit_delay_ms=102.23                                      │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│  KAFKA TOPIC (dht22-measurements)                               │
│  [Message Queue - No direct metrics]                             │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│  FLINK CONSUMER (streamanalyticsapp_instrumented.py)            │
│  ✓ kafka_consumed=1000                                           │
│  ✓ kafka_consumption_elapsed_sec=5.32                            │
│  ✓ kafka_consumption_rate_events_sec=187.97                      │
│  ✓ windows_emitted=6745                                          │
│  ✓ avg_window_size=14.2                                          │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│  FLINK WINDOW & AGGREGATE                                       │
│  ✓ window_sizes=[13, 14, 15, 14, ...]                            │
│  ✓ windows_processed=6745                                        │
│  ✓ avg_window_size=14.2                                          │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│  CASSANDRA SINK (CallbackMapFunction.map)                       │
│  ✓ cassandra_writes_successful=6745                              │
│  ✓ cassandra_writes_failed=0                                     │
│  ✓ cassandra_avg_write_ms=1.23                                   │
│  ✓ cassandra_max_write_ms=8.92                                   │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│              CASSANDRA TABLE (stream_analytics_results)          │
│              ✓ Total rows: 6745                                  │
└─────────────────────────────────────────────────────────────────┘

ANALYSIS LAYER:
┌─────────────────────────────────────────────────────────────────┐
│  analyze_performance.py                                         │
│  • Parses all logs                                              │
│  • Generates reports                                            │
│  • Compares test runs                                           │
│  • Exports JSON metrics                                         │
│  • Calculates derived metrics:                                  │
│    - Data loss %                                                │
│    - Amplification factor                                       │
│    - Throughput (in/out)                                        │
│    - E2E pipeline time                                          │
└─────────────────────────────────────────────────────────────────┘
```

---

## Data Collected at Each Stage

### Stage 1: Producer
**File**: `logs/kafka_producer/producer.log`
```
2025-01-15T10:30:00Z INFO producer start source_csv=../data/tenant2/2025-06-01_dht22.csv kafka_brokers=127.0.0.1:9094 topic=dht22-measurements max_events=1000 emit_delay_ms=100
2025-01-15T10:30:01Z PROGRESS produced=100 elapsed_sec=10.05 rate_events_sec=9.95 actual_emit_lag_ms=9950.00
2025-01-15T10:30:02Z PROGRESS produced=200 elapsed_sec=20.10 rate_events_sec=9.95 actual_emit_lag_ms=19900.00
...
2025-01-15T10:31:41Z SUMMARY produced=1000 skipped=0 elapsed_sec=100.90 rate_events_sec=9.91 actual_emit_lag_total_ms=99100.00 avg_emit_delay_ms=99.10
```

**Key Metrics**:
- Total produced events
- Total skipped events
- **Actual production time** (wall-clock)
- **Production rate** (events per second)
- **Emit lag** (first to last event time)
- **Average delay between events**

### Stage 2: Kafka Consumer
**File**: `logs/streamanalyticsapp/metrics.log`
```
2025-01-15T10:31:42Z KAFKA_METRICS consumed=1000 elapsed_sec=5.32 rate_events_sec=187.97
```

**Key Metrics**:
- Total events consumed from Kafka
- **Consumption time** (wall-clock)
- **Consumption rate** (events per second)

### Stage 3: Window Processing
**File**: `logs/streamanalyticsapp/metrics.log`
```
2025-01-15T10:31:45Z window_size_avg=14.2 windows_processed=100
2025-01-15T10:31:48Z window_size_avg=14.1 windows_processed=200
...
2025-01-15T10:32:15Z window_size_avg=14.2 windows_processed=6745
```

**Key Metrics**:
- **Average records per window**
- **Total windows emitted**
- **Minimum/maximum window size** (if tracked)

### Stage 4: Cassandra Writes
**File**: `logs/streamanalyticsapp/metrics.log`
```
2025-01-15T10:31:47Z cassandra_writes=500 avg_write_ms=1.23 max_write_ms=5.67
2025-01-15T10:31:52Z cassandra_writes=1000 avg_write_ms=1.21 max_write_ms=7.42
...
2025-01-15T10:32:15Z cassandra_writes=6745 avg_write_ms=1.22 max_write_ms=8.92
```

**Key Metrics**:
- **Cassandra write rate** (cumulative)
- **Average write latency** (milliseconds per write)
- **Maximum write latency** (outliers/spikes)

### Stage 5: Summary
**File**: `logs/streamanalyticsapp/metrics.log`
```
================================================================================
FINAL PERFORMANCE SUMMARY
================================================================================
kafka_events_consumed=1000
kafka_consumption_elapsed_sec=5.32
kafka_consumption_rate_events_sec=187.97
windows_emitted=6745
avg_window_size=14.2
cassandra_writes_successful=6745
cassandra_writes_failed=0
cassandra_avg_write_ms=1.22
cassandra_max_write_ms=8.92
cassandra_min_write_ms=0.45
amplification_factor=6.75
total_pipeline_time_sec=112.43
================================================================================
```

**Key Metrics**:
- **Data loss** (produced vs consumed)
- **Amplification** (output rows / input events)
- **End-to-end latency** (seconds)

---

## How to Interpret Key Metrics

### 1. Production Rate vs Consumption Rate

**Question**: Is Flink keeping up with the producer?

```
Example A2 (MODERATE):
  Producer Rate: 9.91 events/sec
  Consumer Rate: 187.97 events/sec  ← Much faster (batch consumption)
  
  ✓ HEALTHY: Flink processes events faster than they arrive
```

```
Example A1 (BURST):
  Producer Rate: 200+ events/sec
  Consumer Rate: 150 events/sec  ← Slightly slower
  
  ⚠ WATCH: Consumer might fall behind if producer is much faster
```

### 2. Average Window Size

**Question**: Are windows getting the expected number of events?

```
15-minute window with 1-minute slide = typically 15 min of data
If events are at ~1 per minute: avg_window_size ≈ 15

B2 (15min/1min slide):
  avg_window_size = 14.2  ✓ Good (close to 15)

B4 (15min/10sec slide):
  avg_window_size = 14.1  ✓ Good (similar, despite frequent emission)
```

If avg_window_size is too low → events are being dropped or arriving out of order

### 3. Cassandra Write Latency

**Question**: Is Cassandra writes becoming a bottleneck?

```
avg_write_ms = 0.8-1.5
  ✓ HEALTHY: Single node baseline

avg_write_ms = 2-5
  ⚠ ACCEPTABLE: Some variance normal

avg_write_ms > 10
  ✗ PROBLEM: Likely bottleneck

max_write_ms >> avg_write_ms (e.g., max=50, avg=1.2)
  ⚠ OUTLIER: GC pause or network hiccup
```

### 4. Amplification Factor

**Question**: Are all windows being generated as expected?

Expected by window config:
- B1 (5m/30s): 5×60/30 = 10 windows per event → ~10x amplification
- B2 (15m/1m): 15×60/60 = 15 windows per event → ~15x amplification
- B3 (30m/5m): 30/5 = 6 windows per event → ~6x amplification

```
B2 Expected: ~15x
B2 Actual: 6.75x
  
  ✗ PROBLEM: Only half the expected windows!
  → Likely cause: Late arriving events (dropped by watermark)
  → Solution: Increase OUT_OF_ORDER_MINUTES
```

### 5. Data Loss

**Question**: Are events being dropped?

```
Produced: 1000
Consumed: 1000
Cassandra: 6745

Loss = (1000 - 1000) / 1000 = 0%
  ✓ PERFECT: No data loss from producer to Kafka
  ✓ Amplification (6745 / 1000 = 6.75) matches B2 window config
```

```
Produced: 1000
Consumed: 950
Loss = 50 / 1000 = 5%
  ✗ PROBLEM: 50 events lost
  → Cause: KAFKA_IDLE_TIMEOUT_MS too short
  → Check: grep "skipped\|consumed" logs/streamanalyticsapp/app.log
```

### 6. Total Pipeline Time

**Question**: How long does the whole process take?

```
100 events at MODERATE speed (100ms delay):
  Expected: ~10 seconds (100 × 100ms = 10s)
  Actual: 112 seconds
  
  Breakdown:
  - Producer: 10 seconds (as expected)
  - Kafka consume: 0.5 seconds (data already available)
  - Window processing: 1 second
  - Cassandra writes: 0.5 seconds
  - Other: 90 seconds (????)
  
  → Likely: Java startup time dominates
  → Solution: Run multiple tests; first run always slower
```

---

## Accessing the Metrics

### Option 1: Read Raw Logs
```bash
# Producer summary
tail -1 logs/kafka_producer/producer.log

# Flink metrics
tail -20 logs/streamanalyticsapp/metrics.log

# Final summary
grep "FINAL PERFORMANCE SUMMARY" -A 20 logs/streamanalyticsapp/metrics.log
```

### Option 2: Generate Report
```bash
python auxx/analyze_performance.py --test-name="my_test"

# Outputs:
# - results/my_test_report.txt (human readable)
# - results/my_test_metrics.json (machine readable)
```

### Option 3: Compare Multiple Tests
```bash
python auxx/analyze_performance.py

# Generates comparison table:
# Test Name         | Produced | Consumed | Windows | Cassandra | Prod Rate | ...
# A1_burst          |     1000 |     1000 |    6800 |      6800 |    200.50 | ...
# A2_moderate       |     1000 |     1000 |    6745 |      6745 |      9.70 | ...
# B1_small_windows  |     1000 |     1000 |   15230 |     15230 |      9.70 | ...
```

### Option 4: Export to CSV
```bash
# Generate and export comparison metrics
python auxx/analyze_performance.py | tee results/comparison.txt

# Convert to CSV programmatically
python -c "
import json
import csv
from pathlib import Path

with open('results/comparison.csv', 'w') as f:
    writer = csv.DictWriter(f, fieldnames=['test', 'produced', 'consumed', 'cassandra', 'amplification'])
    writer.writeheader()
    for metrics_file in Path('results').glob('*_metrics.json'):
        with open(metrics_file) as mf:
            data = json.load(mf)
            writer.writerow({
                'test': data['test_name'],
                'produced': data['producer']['produced'],
                'consumed': data['flink']['kafka_consumed'],
                'cassandra': data['cassandra_row_count'],
                'amplification': data['flink']['amplification_factor'],
            })
"
```

---

## What's Ready vs What Needs Manual Setup

### ✅ Ready to Use (New Files Created)

1. **Instrumented Producer**: `kafka_csv_producer_instrumented.py`
   - Just swap the name in command: `python kafka_csv_producer_instrumented.py`

2. **Instrumented Flink App**: `streamanalyticsapp_instrumented.py`
   - Just swap the name: `python streamanalyticsapp_instrumented.py`

3. **Analysis Script**: `auxx/analyze_performance.py`
   - Just run: `python auxx/analyze_performance.py --test-name="A1"`

4. **Test Harness**: `auxx/run_performance_tests.sh`
   - Just run: `bash auxx/run_performance_tests.sh`

### 📋 Configuration (Existing, No Changes Needed)

- `EMIT_DELAY_MS` - Already supported in original producer
- `MAX_EVENTS` - Already supported in original producer
- `KAFKA_IDLE_TIMEOUT_MS` - Already supported in original Flink app
- Window parameters - In Flink at line 427, need manual edit per test

### ⚙️ Manual Setup Required (For Window Variation Tests B1, B3, B4)

Edit [streamanalyticsapp_instrumented.py line 427](streamanalyticsapp_instrumented.py#L427):

```python
# B1: Small windows
.window(SlidingEventTimeWindows.of(Time.minutes(5), Time.seconds(30)))

# B2: Baseline (default)
.window(SlidingEventTimeWindows.of(Time.minutes(15), Time.minutes(1)))

# B3: Large windows
.window(SlidingEventTimeWindows.of(Time.minutes(30), Time.minutes(5)))

# B4: High-frequency
.window(SlidingEventTimeWindows.of(Time.minutes(15), Time.seconds(10)))
```

---

## Performance Metrics Checklist

- [x] Producer timing and rates
- [x] Kafka consumption timing and rates
- [x] Window size metrics
- [x] Cassandra write latency (average and max)
- [x] Cassandra error tracking
- [x] End-to-end pipeline timing
- [x] Data loss calculation
- [x] Amplification factor calculation
- [x] Automatic report generation
- [x] Comparison table generation
- [x] JSON export for further analysis
- [x] Progress logging (every 100 events/500 writes)
- [ ] Throughput curves over time (future)
- [ ] Memory usage tracking (future)
- [ ] GC pause detection (future)
- [ ] Flink checkpoint metrics (future)

---

## Next Steps

1. **Run a baseline test**:
   ```bash
   bash auxx/run_performance_tests.sh --only=A2
   ```

2. **Review the report**:
   ```bash
   cat results/A2_moderate_report.txt
   ```

3. **Run all scenarios**:
   ```bash
   bash auxx/run_performance_tests.sh
   ```

4. **Analyze trade-offs** using the comparison table to choose optimal window configuration

5. **Optimize** based on findings (parallelism, batch size, resources)

For detailed testing procedures, see [PERFORMANCE_TESTING_GUIDE.md](PERFORMANCE_TESTING_GUIDE.md)
For comprehensive test plan, see [TESTING_PLAN_PERFORMANCE.md](TESTING_PLAN_PERFORMANCE.md)
