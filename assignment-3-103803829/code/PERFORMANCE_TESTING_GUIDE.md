# Performance Testing Guide - Stream Analytics Application

This document describes the comprehensive performance testing setup for the stream analytics application, including:
- Current performance metrics available
- New instrumented versions of producer and Flink app
- Performance analysis tools
- How to run test scenarios

## Quick Start

### Run Full Test Suite (All Scenarios A1-A3, B1-B4)
```bash
cd assignment-3-103803829/code
source .venv311/bin/activate
bash auxx/run_performance_tests.sh
```

### Run Specific Test
```bash
bash auxx/run_performance_tests.sh --only=A1,A2,A3  # Streaming speed tests only
bash auxx/run_performance_tests.sh --only=B2         # Baseline window config only
bash auxx/run_performance_tests.sh --skip-infra      # Reuse existing infra
```

### Generate Report After Manual Tests
```bash
python auxx/analyze_performance.py --test-name="my_test_name"
```

---

## Performance Metrics Available

### 1. Current State (Existing Code)

#### Producer (`kafka_csv_producer.py`)
- **Collected**: Event count, skip count
- **Missing**: Timing data

#### Flink App (`streamanalyticsapp.py`)
- **Collected**: Config logging, error logging
- **Missing**: Consumption rates, window metrics, write latency

#### Cassandra
- **Collected**: Manual query of row count
- **Missing**: Write timing, error tracking in app

### 2. New Instrumented Versions

Two new files provide detailed metrics:
- `kafka_csv_producer_instrumented.py` - Enhanced producer with timing
- `streamanalyticsapp_instrumented.py` - Enhanced Flink app with comprehensive metrics

#### Producer Metrics (`kafka_csv_producer_instrumented.py`)
```
SUMMARY produced=400 skipped=0 elapsed_sec=41.24 rate_events_sec=9.70 actual_emit_lag_total_ms=40891.51 avg_emit_delay_ms=102.23
```

Tracks:
- Total events produced
- Total events skipped (malformed)
- **Production time** (wall-clock)
- **Production rate** (events/sec)
- **Actual emit lag** (first to last event)
- **Average emit delay** (actual vs configured)

#### Kafka Consumer Metrics (`streamanalyticsapp_instrumented.py`)
```
KAFKA_METRICS consumed=400 elapsed_sec=5.32 rate_events_sec=75.19
```

Tracks:
- Events consumed from Kafka
- **Consumption time**
- **Consumption rate** (events/sec)

#### Window Processing Metrics
```
window_size_avg=13.4 windows_processed=100
```

Tracks:
- Average records per window
- Total windows processed
- Distribution of window sizes

#### Cassandra Write Metrics
```
cassandra_writes=500 avg_write_ms=1.23 max_write_ms=5.67
```

Tracks per batch:
- Number of successful writes
- **Average write latency** (ms)
- **Max write latency** (ms)

And final summary:
```
cassandra_writes_successful=5985
cassandra_writes_failed=0
cassandra_avg_write_ms=1.15
cassandra_max_write_ms=8.92
amplification_factor=14.96
```

#### End-to-End Metrics
```
total_pipeline_time_sec=47.23
```

Tracks:
- Full pipeline execution time
- Data loss percentage
- Overall throughput (events/sec in and out)

---

## Performance Test Scenarios

### Scenario A: Varying Streaming Speed

#### A1: Burst Mode (No Delay)
- **Configuration**: `EMIT_DELAY_MS=0`, `MAX_EVENTS=1000`
- **Expected**: Fastest producer, highest Kafka throughput
- **Metrics**: Baseline for max speed

#### A2: Moderate Delay (Realistic)
- **Configuration**: `EMIT_DELAY_MS=100`, `MAX_EVENTS=1000`
- **Expected**: ~10 events/second (realistic sensor rate)
- **Metrics**: Stable consumption, balanced Cassandra writes

#### A3: Slow Streaming (Low Stress)
- **Configuration**: `EMIT_DELAY_MS=1000`, `MAX_EVENTS=600`
- **Expected**: ~1 event/second, minimal resource usage
- **Metrics**: Lowest latency per event, most stable writes

### Scenario B: Varying Window Parameters

#### B1: Small Windows (5-min / 30-sec slide)
- **Window**: Highest amplification (~30x)
- **Use case**: Very responsive alerts, high state memory

#### B2: Baseline Windows (15-min / 1-min slide)
- **Window**: Production default (~6-7x amplification)
- **Use case**: Balance between responsiveness and load

#### B3: Large Windows (30-min / 5-min slide)
- **Window**: Lowest amplification (~6x)
- **Use case**: Long-term trend analysis, minimal write load

#### B4: High-Frequency Slides (15-min / 10-sec slide)
- **Window**: Extreme amplification (~90x)
- **Use case**: Near-real-time monitoring, stress test

---

## Detailed Metrics Collection Points

### Producer Timeline
```
[START] → [EVENT 1] → [EMIT_DELAY] → [EVENT 2] → ... → [EVENT N] → [FLUSH] → [END]
 |                                                                      |
 └─ elapsed_sec ────────────────────────────────────────────────────────┘
    = total production time

 |                     actual_emit_lag_total_ms
 └─ (timestamp of last event - timestamp of first event)
```

### Kafka Consumer Timeline
```
[SUBSCRIBE] → [POLL 1] → [POLL 2] → ... → [TIMEOUT/MAX_EVENTS] → [CLOSE]
 |                                                                  |
 └─ kafka_consumption_elapsed_sec ──────────────────────────────────┘
    = consumption time (including Kafka overhead)

rate = consumed_events / elapsed_sec
```

### Flink Pipeline Timeline
```
[LOAD EVENTS]    [WINDOWS]      [WRITE]
       ↓             ↓              ↓
    Kafka       Flink State    Cassandra
   (5 sec)      (5 sec)        (5 sec)
       ↓             ↓              ↓
   < 5-10 events/sec > <avg 1-2ms per write>
```

### Cassandra Write Timeline (Per Window)
```
[CASSANDRA READY] → [START WRITE] → [EXECUTE STMT] → [COMMIT]
      (setup)              ↓          (1-8ms)        (end)
                    write_start_ms
                           ← write_elapsed_ms →
```

---

## How to Interpret Results

### Key Performance Indicators

#### 1. Production vs Consumption Rate
```
If producer_rate >> consumer_rate:
  ✗ Kafka is bottleneck (rare)
  ✗ Flink can't keep up (may need parallelism)

If producer_rate ≈ consumer_rate:
  ✓ System is balanced

If producer_rate < consumer_rate:
  ✓ Flink is efficient (processing faster than input)
```

#### 2. Amplification Factor
```
Amplification = Cassandra_Rows / Input_Events

B2 (15m/1m):  ~15x = expected
B1 (5m/30s):  ~30x = expected
B3 (30m/5m):  ~6x = expected
B4 (15m/10s): ~90x = heavy load

If amplification < expected:
  ✗ Windows are missing (late arriving events)
  ✗ Event time issues

If amplification > expected:
  ✗ Large parallelism creating duplicate windows
```

#### 3. Cassandra Write Latency
```
avg_write_ms < 2.0
  ✓ Healthy (single node baseline)

avg_write_ms = 1-5
  ✓ Good (some variance acceptable)

avg_write_ms > 10
  ✗ Potential bottleneck (too many concurrent writes)

max_write_ms >> avg_write_ms
  ✗ Outliers (GC pauses, network hiccups)
```

#### 4. Data Loss Check
```
Loss % = (produced - consumed) / produced × 100

Loss == 0%
  ✓ Perfect (no backpressure, no drops)

Loss < 1%
  ✓ Acceptable (minimal)

Loss > 5%
  ✗ Investigate (KAFKA_IDLE_TIMEOUT_MS too short?)
```

---

## Performance Analysis Tools

### 1. Per-Test Report Generation

After running a test, generate detailed report:
```bash
python auxx/analyze_performance.py --test-name="A2_moderate"
```

Output:
- `results/A2_moderate_report.txt` - Human-readable summary
- `results/A2_moderate_metrics.json` - Machine-readable metrics

#### Report Contents
```
PRODUCER METRICS
  Produced: 1000 events
  Production Rate: 9.70 events/sec
  Total Emit Lag: 40891 ms

KAFKA CONSUMER METRICS (Flink)
  Events Consumed: 1000 events
  Consumption Rate: 75.19 events/sec
  
WINDOW PROCESSING METRICS
  Windows Emitted: 6745
  Avg Window Size: 14.2 records/window
  Amplification Factor: 6.75x

CASSANDRA SINK METRICS
  Successful Writes: 6745
  Failed Writes: 0
  Avg Write Latency: 1.23 ms

END-TO-END METRICS
  Total Pipeline Time: 47.23 seconds
  Input Throughput: 21.17 events/sec
  Output Throughput: 142.78 rows/sec
```

### 2. Comparison Table Generation

Automatically generated after all tests:
```
Test Name         | Produced | Consumed | Windows | Cassandra | Prod Rate | Cons Rate | Avg Write | Amplif | Total Time
A1_burst          |     1000 |     1000 |    6800 |      6800 |    200.50 |    157.32 |      1.42 |   6.80 |      6.12
A2_moderate       |     1000 |     1000 |    6745 |      6745 |      9.70 |     75.19 |      1.23 |   6.75 |     47.23
A3_slow           |      600 |      600 |    4080 |      4080 |      1.05 |      8.32 |      0.98 |   6.80 |    614.51
```

---

## Instrumentation Code Overview

### Producer Instrumentation
File: `kafka_csv_producer_instrumented.py`

Key additions:
```python
producer_start_time = time.time()
first_event_time = None
last_event_time = None

# ... produce loop ...

for row in reader:
    # ... produce event ...
    produced += 1
    last_event_time = time.time()
    if first_event_time is None:
        first_event_time = last_event_time
    
    if produced % 100 == 0:
        elapsed = time.time() - producer_start_time
        rate = produced / elapsed
        # Log progress

producer_end_time = time.time()
total_elapsed = producer_end_time - producer_start_time
production_rate = produced / total_elapsed
```

### Flink Instrumentation
File: `streamanalyticsapp_instrumented.py`

Key additions:
```python
# Global metrics dictionary
METRICS = {
    "kafka_consumption_start": None,
    "kafka_consumption_end": None,
    "kafka_events_consumed": 0,
    "windows_emitted": 0,
    "cassandra_writes_successful": 0,
    "cassandra_writes_failed": 0,
    "cassandra_write_times_ms": [],
    "window_sizes": [],
    "pipeline_start": None,
}

# In load_events_from_kafka()
METRICS["kafka_consumption_start"] = time.time()
# ... consume events ...
METRICS["kafka_consumption_end"] = time.time()
METRICS["kafka_events_consumed"] = len(events)

# In CallbackMapFunction.map()
write_start_ms = time.time() * 1000
# ... cassandra write ...
write_elapsed_ms = (time.time() * 1000) - write_start_ms
METRICS["cassandra_write_times_ms"].append(write_elapsed_ms)

# In AnalyticsWindowFunction.process()
METRICS["window_sizes"].append(len(records))
METRICS["windows_emitted"] += 1

# At end of main()
# Generate comprehensive metrics summary
```

---

## Expected Performance Baselines

Based on single-node setup (parallelism=1):

### Producer Versions
| Speed | Events/sec | Configuration |
|-------|-----------|---|
| Burst | 200+ | `EMIT_DELAY_MS=0` |
| Moderate | 10 | `EMIT_DELAY_MS=100` |
| Slow | 1 | `EMIT_DELAY_MS=1000` |

### Flink Consumption
| Configuration | Rate | Processing Time |
|---|---|---|
| No delay | ~150 ev/sec | ~6 sec for 1000 events |
| 100ms delay | ~75 ev/sec | Variable (depends on producer) |
| 1000ms delay | ~10 ev/sec | ~100+ sec |

### Cassandra Writes
| Scenario | Avg Latency | Max Latency | Error Rate |
|---|---|---|---|
| A1 (burst) | 1.2-1.5 ms | 5-8 ms | 0% |
| A2 (moderate) | 1.0-1.2 ms | 3-5 ms | 0% |
| A3 (slow) | 0.8-1.0 ms | 2-3 ms | 0% |
| B4 (high freq) | 1.5-2.5 ms | 8-15 ms | ~0% |

---

## Troubleshooting Tests

### Producer completes but Flink finds no events
- **Cause**: Kafka idle timeout too short
- **Fix**: Increase `KAFKA_IDLE_TIMEOUT_MS` to 30000
- **Check**: `grep "no valid events" logs/streamanalyticsapp/app.log`

### High Cassandra write errors
- **Cause**: Network timeouts or schema mismatch
- **Fix**: Check Cassandra is running: `docker ps | grep cassandra`
- **Check**: `grep "cassandra write failed" logs/streamanalyticsapp/app.log | wc -l`

### Amplification factor lower than expected
- **Cause**: Windows being dropped (event time out of order)
- **Fix**: Increase `OUT_OF_ORDER_MINUTES`
- **Check**: Look at `actual_emit_lag_total_ms` in producer metrics

### Test runs very slow
- **Cause**: Flink Java process initialization
- **Fix**: First run is always slower; rerun for consistent results
- **Check**: `total_pipeline_time_sec` should be consistent on repeated runs

---

## Continuous Monitoring

To monitor a live test in real time:

```bash
# Terminal 1: Start test
bash auxx/run_performance_tests.sh --only=A2

# Terminal 2: Monitor metrics
tail -f logs/streamanalyticsapp/metrics.log

# Terminal 3: Monitor Cassandra growth
watch 'docker exec cassandra1 cqlsh -e "SELECT count(*) FROM mysimbdp_tenant2.stream_analytics_results;"'
```

---

## Exporting Results

### Generate Comparison CSV
```bash
python -c "
import json
from pathlib import Path

metrics = {}
for f in Path('results').glob('*_metrics.json'):
    with open(f) as fp:
        data = json.load(fp)
        test_name = f.stem.replace('_metrics', '')
        metrics[test_name] = {
            'produced': data['producer']['produced'],
            'consumed': data['flink']['kafka_consumed'],
            'cassandra': data['cassandra_row_count'],
            'prod_rate': data['producer']['rate_events_sec'],
            'cons_rate': data['flink']['kafka_rate_events_sec'],
            'avg_write_ms': data['flink']['cassandra_avg_write_ms'],
            'amplification': data['flink']['amplification_factor'],
            'total_time': data['flink']['total_pipeline_time_sec'],
        }

import csv
with open('results/comparison.csv', 'w') as f:
    writer = csv.DictWriter(f, fieldnames=['test_name'] + list(list(metrics.values())[0].keys()))
    writer.writeheader()
    for name, data in sorted(metrics.items()):
        row = {'test_name': name}
        row.update(data)
        writer.writerow(row)

print('Saved to results/comparison.csv')
"
```

---

## Next Steps

1. **Run baseline test** with instrumented versions to establish current performance
2. **Run all scenarios** (A1-A3, B1-B4) to understand trade-offs
3. **Identify bottlenecks** from metrics comparison
4. **Optimize** (parallelism, batch size, window size) based on findings
5. **Re-benchmark** to validate improvements

For detailed performance optimization recommendations, see the performance test report generated after running the full test suite.
