#!/usr/bin/env python3
"""
Performance test analysis and report generation.

Parses metrics from producer, flink app, and cassandra to generate comparative performance reports.

Usage:
    python auxx/analyze_performance.py [--test-name="A1_burst"] [--cassandra-rows=5985]
"""

import json
import re
import subprocess
import sys
from pathlib import Path
from collections import defaultdict
from datetime import datetime


def parse_producer_log(log_path: Path) -> dict:
    """Extract producer metrics from log"""
    metrics = {
        "produced": 0,
        "skipped": 0,
        "elapsed_sec": 0,
        "rate_events_sec": 0,
        "actual_emit_lag_total_ms": 0,
        "avg_emit_delay_ms": 0,
        "log_file": str(log_path),
    }

    if not log_path.exists():
        return metrics

    with open(log_path) as f:
        for line in f:
            if "SUMMARY" in line:
                # SUMMARY produced=400 skipped=0 elapsed_sec=41.24 rate_events_sec=9.70 actual_emit_lag_total_ms=40891.51 avg_emit_delay_ms=102.23
                match = re.search(
                    r"produced=(\d+).*skipped=(\d+).*elapsed_sec=([\d.]+).*rate_events_sec=([\d.]+)"
                    r".*actual_emit_lag_total_ms=([\d.]+).*avg_emit_delay_ms=([\d.]+)",
                    line,
                )
                if match:
                    metrics["produced"] = int(match.group(1))
                    metrics["skipped"] = int(match.group(2))
                    metrics["elapsed_sec"] = float(match.group(3))
                    metrics["rate_events_sec"] = float(match.group(4))
                    metrics["actual_emit_lag_total_ms"] = float(match.group(5))
                    metrics["avg_emit_delay_ms"] = float(match.group(6))

    return metrics


def parse_flink_metrics_log(log_path: Path) -> dict:
    """Extract flink metrics from dedicated metrics log"""
    metrics = {
        "kafka_consumed": 0,
        "kafka_elapsed_sec": 0,
        "kafka_rate_events_sec": 0,
        "windows_emitted": 0,
        "windows_processed": 0,
        "avg_window_size": 0,
        "cassandra_writes_successful": 0,
        "cassandra_writes_failed": 0,
        "cassandra_avg_write_ms": 0,
        "cassandra_max_write_ms": 0,
        "cassandra_min_write_ms": 0,
        "amplification_factor": 0,
        "total_pipeline_time_sec": 0,
        "errors": [],
        "log_file": str(log_path),
    }

    if not log_path.exists():
        return metrics

    with open(log_path) as f:
        for line in f:
            # KAFKA_METRICS consumed=400 elapsed_sec=5.32 rate_events_sec=75.19
            if "KAFKA_METRICS" in line:
                match = re.search(
                    r"consumed=(\d+).*elapsed_sec=([\d.]+).*rate_events_sec=([\d.]+)",
                    line,
                )
                if match:
                    metrics["kafka_consumed"] = int(match.group(1))
                    metrics["kafka_elapsed_sec"] = float(match.group(2))
                    metrics["kafka_rate_events_sec"] = float(match.group(3))

            # cassandra_writes=500 avg_write_ms=1.23 max_write_ms=5.67
            if "cassandra_writes=" in line and "avg_write_ms=" in line:
                match = re.search(
                    r"cassandra_writes=(\d+).*avg_write_ms=([\d.]+).*max_write_ms=([\d.]+)",
                    line,
                )
                if match:
                    metrics["cassandra_writes_successful"] = int(match.group(1))
                    metrics["cassandra_avg_write_ms"] = float(match.group(2))
                    metrics["cassandra_max_write_ms"] = float(match.group(3))

            # windows_emitted=5985
            if "windows_emitted=" in line:
                match = re.search(r"windows_emitted=(\d+)", line)
                if match:
                    metrics["windows_emitted"] = int(match.group(1))

            # window_size_avg=1.0 windows_processed=100
            if "windows_processed=" in line:
                match = re.search(r"windows_processed=(\d+)", line)
                if match:
                    metrics["windows_processed"] = max(
                        metrics["windows_processed"], int(match.group(1))
                    )

            # avg_window_size=13.4
            if "avg_window_size=" in line:
                match = re.search(r"avg_window_size=([\d.]+)", line)
                if match:
                    metrics["avg_window_size"] = float(match.group(1))

            # cassandra_writes_failed=5
            if "cassandra_writes_failed=" in line:
                match = re.search(r"cassandra_writes_failed=(\d+)", line)
                if match:
                    metrics["cassandra_writes_failed"] = int(match.group(1))

            # amplification_factor=14.96
            if "amplification_factor=" in line:
                match = re.search(r"amplification_factor=([\d.]+)", line)
                if match:
                    metrics["amplification_factor"] = float(match.group(1))

            # total_pipeline_time_sec=47.23
            if "total_pipeline_time_sec=" in line:
                match = re.search(r"total_pipeline_time_sec=([\d.]+)", line)
                if match:
                    metrics["total_pipeline_time_sec"] = float(match.group(1))

    return metrics


def parse_flink_app_log(log_path: Path) -> dict:
    """Extract cassandra errors from app log"""
    metrics = {
        "cassandra_write_errors": 0,
        "callback_errors": 0,
        "consumed_total": 0,
    }

    if not log_path.exists():
        return metrics

    with open(log_path) as f:
        for line in f:
            if "cassandra write failed" in line:
                metrics["cassandra_write_errors"] += 1
            if "callback failed" in line:
                metrics["callback_errors"] += 1
            if "consumed" in line and "events from kafka" in line:
                match = re.search(r"consumed (\d+) events", line)
                if match:
                    metrics["consumed_total"] = int(match.group(1))

    return metrics


def query_cassandra_count(keyspace: str = "mysimbdp_tenant2", table: str = "stream_analytics_results") -> int:
    """Query cassandra for row count"""
    try:
        result = subprocess.run(
            [
                "docker",
                "exec",
                "cassandra1",
                "cqlsh",
                "-e",
                f"SELECT count(*) FROM {keyspace}.{table};",
            ],
            capture_output=True,
            text=True,
            timeout=10,
        )
        if result.returncode != 0:
            print(f"Cassandra query failed: {result.stderr}", file=sys.stderr)
            return 0

        # Parse output: should contain count on a line
        for line in result.stdout.split("\n"):
            if line.strip().isdigit():
                return int(line.strip())
    except Exception as e:
        print(f"ERROR querying Cassandra: {e}", file=sys.stderr)

    return 0


def count_analytics_output_lines(log_path: Path) -> int:
    """Count JSON lines in analytics output"""
    if not log_path.exists():
        return 0

    with open(log_path) as f:
        count = 0
        for line in f:
            stripped = line.strip()
            if not stripped:
                continue
            # Supports both plain JSONL and timestamp-prefixed JSON payload lines.
            if stripped.startswith("{") or " {" in stripped:
                count += 1
        return count


def generate_report(test_name: str = "Test", result_dir: Path = Path("results")) -> None:
    """Generate performance report"""

    result_dir.mkdir(exist_ok=True)

    # Paths to log files
    producer_log = Path("logs/kafka_producer/producer.log")
    flink_app_log = Path("logs/streamanalyticsapp/app.log")
    flink_metrics_log = Path("logs/streamanalyticsapp/metrics.log")
    analytics_output = Path("logs/streamanalyticsapp/analytics_output.jsonl")

    # Parse all metrics
    producer_metrics = parse_producer_log(producer_log)
    flink_app_metrics = parse_flink_app_log(flink_app_log)
    flink_metrics = parse_flink_metrics_log(flink_metrics_log)
    cassandra_row_count = query_cassandra_count()
    analytics_lines = count_analytics_output_lines(analytics_output)

    # Derive resilient metrics in case PyFlink worker-side counters do not reflect in main process metrics.
    if flink_metrics["kafka_consumed"] == 0 and flink_app_metrics["consumed_total"] > 0:
        flink_metrics["kafka_consumed"] = flink_app_metrics["consumed_total"]

    if flink_metrics["windows_emitted"] == 0 and analytics_lines > 0:
        flink_metrics["windows_emitted"] = analytics_lines

    if flink_metrics["windows_emitted"] == 0 and flink_metrics["windows_processed"] > 0:
        flink_metrics["windows_emitted"] = flink_metrics["windows_processed"]

    if flink_metrics["cassandra_writes_successful"] == 0 and cassandra_row_count > 0:
        flink_metrics["cassandra_writes_successful"] = cassandra_row_count

    if flink_metrics["cassandra_writes_failed"] == 0 and flink_app_metrics["cassandra_write_errors"] > 0:
        flink_metrics["cassandra_writes_failed"] = flink_app_metrics["cassandra_write_errors"]

    if flink_metrics["amplification_factor"] == 0 and flink_metrics["kafka_consumed"] > 0:
        flink_metrics["amplification_factor"] = (
            flink_metrics["windows_emitted"] / flink_metrics["kafka_consumed"]
        )

    # Generate report
    report = f"""
{'='*80}
PERFORMANCE TEST REPORT: {test_name}
Generated: {datetime.now().isoformat()}
{'='*80}

1. PRODUCER METRICS
{'-'*80}
  Produced:              {producer_metrics['produced']} events
  Skipped:               {producer_metrics['skipped']} events
  Production Time:       {producer_metrics['elapsed_sec']:.2f} seconds
  Production Rate:       {producer_metrics['rate_events_sec']:.2f} events/sec
  Total Emit Lag:        {producer_metrics['actual_emit_lag_total_ms']:.0f} ms
  Avg Emit Delay:        {producer_metrics['avg_emit_delay_ms']:.2f} ms

2. KAFKA CONSUMER METRICS (Flink)
{'-'*80}
  Events Consumed:       {flink_metrics['kafka_consumed']} events
  Consumption Time:      {flink_metrics['kafka_elapsed_sec']:.2f} seconds
  Consumption Rate:      {flink_metrics['kafka_rate_events_sec']:.2f} events/sec
  Errors:                {flink_app_metrics['cassandra_write_errors']}

3. WINDOW PROCESSING METRICS
{'-'*80}
  Windows Emitted:       {flink_metrics['windows_emitted']} windows
  Avg Window Size:       {flink_metrics['avg_window_size']:.1f} records/window
  Amplification Factor:  {flink_metrics['amplification_factor']:.2f}x

4. CASSANDRA SINK METRICS
{'-'*80}
  Write Attempts:        {flink_metrics['cassandra_writes_successful']} successful
  Write Failures:        {flink_metrics['cassandra_writes_failed']} failed
  Avg Write Latency:     {flink_metrics['cassandra_avg_write_ms']:.2f} ms
  Max Write Latency:     {flink_metrics['cassandra_max_write_ms']:.2f} ms
  Actual DB Row Count:   {cassandra_row_count} rows
  Analytics Output Lines:{analytics_lines} lines

5. END-TO-END METRICS
{'-'*80}
  Total Pipeline Time:   {flink_metrics['total_pipeline_time_sec']:.2f} seconds
  Input Events:          {producer_metrics['produced']}
  Output Rows (Cassandra): {cassandra_row_count}
  Throughput (in/sec):   {producer_metrics['produced']/flink_metrics['total_pipeline_time_sec'] if flink_metrics['total_pipeline_time_sec'] > 0 else 0:.2f}
  Throughput (out/sec):  {cassandra_row_count/flink_metrics['total_pipeline_time_sec'] if flink_metrics['total_pipeline_time_sec'] > 0 else 0:.2f}

6. DATA LOSS CHECK
{'-'*80}
    Produced → Consumed:   {producer_metrics['produced']} → {flink_metrics['kafka_consumed']}
  Loss:                  {producer_metrics['produced'] - flink_metrics['kafka_consumed']} events
  Loss %:                {100 * (producer_metrics['produced'] - flink_metrics['kafka_consumed']) / producer_metrics['produced'] if producer_metrics['produced'] > 0 else 0:.2f}%

{'='*80}
"""

    print(report)

    # Save report
    report_file = result_dir / f"{test_name}_report.txt"
    with open(report_file, "w") as f:
        f.write(report)

    # Save metrics as JSON for comparison
    metrics_data = {
        "test_name": test_name,
        "timestamp": datetime.now().isoformat(),
        "producer": producer_metrics,
        "flink_app": flink_app_metrics,
        "flink": flink_metrics,
        "cassandra_row_count": cassandra_row_count,
        "analytics_output_lines": analytics_lines,
    }

    metrics_file = result_dir / f"{test_name}_metrics.json"
    with open(metrics_file, "w") as f:
        json.dump(metrics_data, f, indent=2)

    print(f"\nReport saved to: {report_file}")
    print(f"Metrics JSON saved to: {metrics_file}")


def generate_comparison_table(result_dir: Path = Path("results")) -> None:
    """Generate comparison table for multiple test runs"""

    result_dir.mkdir(exist_ok=True)

    # Find all metrics files
    metrics_files = sorted(result_dir.glob("*_metrics.json"))

    if not metrics_files:
        print("No metrics files found in results directory")
        return

    tests = []
    for metrics_file in metrics_files:
        with open(metrics_file) as f:
            data = json.load(f)
            tests.append(data)

    # Generate comparison table
    header = (
        "Test Name | Produced | Consumed | Windows | Cassandra | "
        "Prod Rate | Cons Rate | Avg Write | Amplif | Total Time"
    )
    separator = "-" * len(header)

    print("\n" + header)
    print(separator)

    for test in tests:
        test_name = test["test_name"]
        produced = test["producer"]["produced"]
        consumed = test["flink"]["kafka_consumed"]
        windows = test["flink"]["windows_emitted"]
        cassandra = test["cassandra_row_count"]
        prod_rate = test["producer"]["rate_events_sec"]
        cons_rate = test["flink"]["kafka_rate_events_sec"]
        avg_write = test["flink"]["cassandra_avg_write_ms"]
        amplif = test["flink"]["amplification_factor"]
        total_time = test["flink"]["total_pipeline_time_sec"]

        print(
            f"{test_name:20} | {produced:8} | {consumed:8} | {windows:7} | {cassandra:9} | "
            f"{prod_rate:9.2f} | {cons_rate:9.2f} | {avg_write:9.2f} | {amplif:6.2f} | {total_time:10.2f}"
        )


if __name__ == "__main__":
    # Parse command line arguments
    test_name = "baseline"
    for arg in sys.argv[1:]:
        if arg.startswith("--test-name="):
            test_name = arg.split("=", 1)[1]

    generate_report(test_name)
    generate_comparison_table()
