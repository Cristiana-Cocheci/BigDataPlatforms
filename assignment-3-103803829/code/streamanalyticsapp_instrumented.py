#!/usr/bin/env python3
"""
Performance instrumented version of streamanalyticsapp.py
Adds timing metrics collection for Kafka consumption, window processing, and Cassandra writes.

Key additions:
- Kafka consumption timing and rate
- Per-window size tracking
- Cassandra write latency (per-batch and aggregate)
- Final performance summary
"""

import json
import os
import sys
import time
from datetime import datetime, timezone
from statistics import median
from typing import Dict, Iterable, List, Tuple
from urllib import request

from cassandra.cluster import Cluster
from confluent_kafka import Consumer

from pyflink.common import Duration, WatermarkStrategy
from pyflink.common.watermark_strategy import TimestampAssigner
from pyflink.common.typeinfo import Types
from pyflink.datastream import StreamExecutionEnvironment
from pyflink.datastream.functions import KeySelector, MapFunction, ProcessWindowFunction
from pyflink.datastream.window import SlidingEventTimeWindows
from pyflink.common.time import Time


# Input event tuple schema:
# (sensor_id, sensor_type, location, lat, lon, event_ts_ms, temperature, humidity)
EventTuple = Tuple[int, str, str, float, float, int, float, float]

DEFAULT_LOG_DIR = "./logs/streamanalyticsapp"
APP_LOG_PATH = ""
ANALYTICS_LOG_PATH = ""
METRICS_LOG_PATH = ""  # NEW: Separate metrics file

DEFAULT_KAFKA_BROKERS = "localhost:9094"
DEFAULT_KAFKA_TOPIC = "dht22-measurements"
DEFAULT_KAFKA_GROUP = "tenant2-flink-analytics"

DEFAULT_CASSANDRA_HOSTS = "127.0.0.1"
DEFAULT_CASSANDRA_KEYSPACE = "mysimbdp_tenant2"

# === NEW: Global metrics counters ===
METRICS = {
    "kafka_consumption_start": None,
    "kafka_consumption_end": None,
    "kafka_consumption_elapsed": 0,
    "kafka_events_consumed": 0,
    "windows_emitted": 0,
    "cassandra_writes_successful": 0,
    "cassandra_writes_failed": 0,
    "cassandra_write_times_ms": [],  # Per-write latencies
    "window_sizes": [],  # Records per window
    "window_processing_start": None,
    "pipeline_start": None,
}


def split_csv(value: str) -> List[str]:
    return [part.strip() for part in value.split(",") if part.strip()]


def init_logging_paths() -> None:
    global APP_LOG_PATH, ANALYTICS_LOG_PATH, METRICS_LOG_PATH

    log_dir = os.getenv("LOG_DIR", DEFAULT_LOG_DIR).strip() or DEFAULT_LOG_DIR
    os.makedirs(log_dir, exist_ok=True)

    APP_LOG_PATH = os.path.join(log_dir, "app.log")
    ANALYTICS_LOG_PATH = os.path.join(log_dir, "analytics_output.jsonl")
    METRICS_LOG_PATH = os.path.join(log_dir, "metrics.log")  # NEW


def append_log(file_path: str, line: str) -> None:
    if not file_path:
        return

    timestamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    with open(file_path, "a", encoding="utf-8") as f:
        f.write(f"{timestamp} {line}\n")


def log_info(message: str) -> None:
    print(message, flush=True)
    append_log(APP_LOG_PATH, message)


def log_warn(message: str) -> None:
    print(message, flush=True)
    append_log(APP_LOG_PATH, message)


def log_metric(message: str) -> None:  # NEW
    print(f"[METRIC] {message}", flush=True)
    append_log(METRICS_LOG_PATH, message)


class SensorKeySelector(KeySelector):
    def get_key(self, value: EventTuple):
        return value[0]


class EventTimestampAssigner(TimestampAssigner):
    def extract_timestamp(self, value: EventTuple, record_timestamp: int) -> int:
        return value[5]


class AnalyticsWindowFunction(ProcessWindowFunction):
    def __init__(self, tenant_id: str, temp_low: float, temp_high: float, hum_low: float, hum_high: float):
        self.tenant_id = tenant_id
        self.temp_low = temp_low
        self.temp_high = temp_high
        self.hum_low = hum_low
        self.hum_high = hum_high

    def process(self, key, context, elements: Iterable[EventTuple]):
        records = list(elements)
        if not records:
            return

        # === NEW: Track window size ===
        METRICS["window_sizes"].append(len(records))
        if len(METRICS["window_sizes"]) % 100 == 0:
            avg_window_size = sum(METRICS["window_sizes"]) / len(METRICS["window_sizes"])
            log_metric(f"window_size_avg={avg_window_size:.1f} windows_processed={len(METRICS['window_sizes'])}")

        sensor_id = records[0][0]
        sensor_type = records[0][1]
        location = records[0][2]
        lat = records[0][3]
        lon = records[0][4]

        temps = [r[6] for r in records]
        humidities = [r[7] for r in records]

        t_min = min(temps)
        t_max = max(temps)
        t_median = median(temps)
        t_avg = sum(temps) / len(temps)

        h_min = min(humidities)
        h_max = max(humidities)
        h_median = median(humidities)
        h_avg = sum(humidities) / len(humidities)

        # Check if any minute is missing
        minutes_in_window = set()
        for record in records:
            event_dt = datetime.fromtimestamp(record[5] / 1000, tz=timezone.utc)
            minutes_in_window.add(event_dt.minute)

        missing_min = 15 - len(minutes_in_window)

        # Alert logic
        is_alert = (
            t_min < self.temp_low
            or t_max > self.temp_high
            or h_min < self.hum_low
            or h_max > self.hum_high
            or missing_min > 0
        )

        window_start = datetime.fromtimestamp(context.window().start / 1000, tz=timezone.utc)
        window_end = datetime.fromtimestamp(context.window().end / 1000, tz=timezone.utc)

        result = {
            "tenant_id": self.tenant_id,
            "sensor_id": sensor_id,
            "sensor_type": sensor_type,
            "location": location,
            "lat": lat,
            "lon": lon,
            "window_start": window_start.strftime("%Y-%m-%dT%H:%M:%SZ"),
            "window_end": window_end.strftime("%Y-%m-%dT%H:%M:%SZ"),
            "t_min": t_min,
            "t_max": t_max,
            "t_median": t_median,
            "t_avg": t_avg,
            "h_min": h_min,
            "h_max": h_max,
            "h_median": h_median,
            "h_avg": h_avg,
            "missing_min": missing_min,
            "is_alert": is_alert,
            "records_in_window": len(records),
        }

        METRICS["windows_emitted"] += 1
        yield json.dumps(result, separators=(",", ":"))


class CallbackMapFunction(MapFunction):
    def __init__(self, callback_url: str, cassandra_hosts: List[str], cassandra_keyspace: str):
        self.callback_url = callback_url
        self.cassandra_hosts = cassandra_hosts
        self.cassandra_keyspace = cassandra_keyspace
        self.cluster = None
        self.session = None
        self.insert_stmt = None

    def _ensure_cassandra_ready(self):
        if self.session is not None and self.insert_stmt is not None:
            return

        self.cluster = Cluster(self.cassandra_hosts)
        self.session = self.cluster.connect()
        self.session.execute(
            (
                f"CREATE KEYSPACE IF NOT EXISTS {self.cassandra_keyspace} "
                "WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}"
            )
        )
        self.session.set_keyspace(self.cassandra_keyspace)
        self.session.execute(
            """
            CREATE TABLE IF NOT EXISTS stream_analytics_results (
                day text,
                hour int,
                sensor_id int,
                window_start timestamp,
                tenant_id text,
                sensor_type text,
                location text,
                lat float,
                lon float,
                window_end timestamp,
                t_min float,
                t_max float,
                t_median float,
                t_avg float,
                h_min float,
                h_max float,
                h_median float,
                h_avg float,
                missing_min int,
                is_alert boolean,
                records_in_window int,
                PRIMARY KEY ((day, hour), sensor_id, window_start)
            )
            """
        )
        self.insert_stmt = self.session.prepare(
            """
            INSERT INTO stream_analytics_results (
                day, hour, sensor_id, window_start, tenant_id, sensor_type, location, lat, lon,
                window_end, t_min, t_max, t_median, t_avg, h_min, h_max, h_median, h_avg,
                missing_min, is_alert, records_in_window
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """
        )

    def map(self, value):
        payload = value if isinstance(value, str) else json.dumps(value, separators=(",", ":"))
        append_log(ANALYTICS_LOG_PATH, payload)

        # === NEW: Cassandra write timing ===
        write_start_ms = time.time() * 1000

        try:
            record = json.loads(payload)
            window_start = datetime.strptime(record["window_start"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
            window_end = datetime.strptime(record["window_end"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
            day = window_start.strftime("%Y-%m-%d")
            hour = int(window_start.strftime("%H"))

            self._ensure_cassandra_ready()
            self.session.execute(
                self.insert_stmt,
                (
                    day,
                    hour,
                    int(record["sensor_id"]),
                    window_start,
                    str(record["tenant_id"]),
                    str(record["sensor_type"]),
                    str(record["location"]),
                    float(record["lat"]),
                    float(record["lon"]),
                    window_end,
                    float(record["t_min"]),
                    float(record["t_max"]),
                    float(record["t_median"]),
                    float(record["t_avg"]),
                    float(record["h_min"]),
                    float(record["h_max"]),
                    float(record["h_median"]),
                    float(record["h_avg"]),
                    int(record["missing_min"]),
                    bool(record["is_alert"]),
                    int(record["records_in_window"]),
                ),
            )

            # === NEW: Record successful write ===
            write_elapsed_ms = (time.time() * 1000) - write_start_ms
            METRICS["cassandra_writes_successful"] += 1
            METRICS["cassandra_write_times_ms"].append(write_elapsed_ms)

            # Log every 500 writes
            if METRICS["cassandra_writes_successful"] % 500 == 0:
                avg_write_ms = sum(METRICS["cassandra_write_times_ms"]) / len(METRICS["cassandra_write_times_ms"])
                max_write_ms = max(METRICS["cassandra_write_times_ms"])
                log_metric(
                    f"cassandra_writes={METRICS['cassandra_writes_successful']} "
                    f"avg_write_ms={avg_write_ms:.2f} max_write_ms={max_write_ms:.2f}"
                )

        except Exception as exc:
            METRICS["cassandra_writes_failed"] += 1
            log_warn(f"WARN cassandra write failed: {exc}")

        if self.callback_url:
            req = request.Request(
                self.callback_url,
                data=payload.encode("utf-8"),
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            try:
                request.urlopen(req, timeout=2)
            except Exception as exc:
                log_warn(f"WARN callback failed: {exc}")

        return payload


def ms_to_iso(ts_ms: int) -> str:
    return datetime.fromtimestamp(ts_ms / 1000, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_kafka_record(record: Dict[str, object]) -> EventTuple:
    required = ["sensor_id", "sensor_type", "location", "lat", "lon", "timestamp", "temperature", "humidity"]
    for key in required:
        if key not in record or record[key] is None or str(record[key]).strip() == "":
            raise ValueError(f"missing or empty kafka field: {key}")

    timestamp = str(record["timestamp"]).strip()
    event_dt = datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
    event_ts_ms = int(event_dt.timestamp() * 1000)

    return (
        int(record["sensor_id"]),
        str(record["sensor_type"]).strip(),
        str(record["location"]).strip(),
        float(record["lat"]),
        float(record["lon"]),
        event_ts_ms,
        float(record["temperature"]),
        float(record["humidity"]),
    )


def load_events_from_kafka(
    kafka_brokers: str,
    kafka_topic: str,
    kafka_group: str,
    max_events: int,
    idle_timeout_ms: int,
) -> List[EventTuple]:
    if max_events < 1:
        max_events = 1000

    # === NEW: Start timing Kafka consumption ===
    METRICS["kafka_consumption_start"] = time.time()

    consumer = Consumer(
        {
            "bootstrap.servers": ",".join(split_csv(kafka_brokers)),
            "group.id": kafka_group,
            "auto.offset.reset": "earliest",
            "enable.auto.commit": True,
            "session.timeout.ms": 10000,
        }
    )
    consumer.subscribe([kafka_topic])

    events: List[EventTuple] = []
    skipped = 0

    deadline = time.time() + (idle_timeout_ms / 1000.0)
    try:
        while time.time() < deadline and len(events) < max_events:
            msg = consumer.poll(1.0)
            if msg is None:
                continue
            if msg.error():
                log_warn(f"WARN kafka poll error: {msg.error()}")
                continue

            try:
                payload = json.loads(msg.value().decode("utf-8"))
                events.append(parse_kafka_record(payload))
            except ValueError as exc:
                skipped += 1
                if skipped <= 5:
                    log_warn(f"WARN skipping malformed kafka message: {exc}")
                continue
    finally:
        consumer.close()

    # === NEW: End timing Kafka consumption ===
    METRICS["kafka_consumption_end"] = time.time()
    METRICS["kafka_consumption_elapsed"] = METRICS["kafka_consumption_end"] - METRICS["kafka_consumption_start"]
    METRICS["kafka_events_consumed"] = len(events)

    if skipped > 0:
        log_info(f"INFO skipped malformed kafka messages: {skipped}")

    if len(events) == 0:
        raise ValueError("no valid events consumed from kafka")

    consumption_rate = len(events) / METRICS["kafka_consumption_elapsed"] if METRICS["kafka_consumption_elapsed"] > 0 else 0
    log_info(f"INFO consumed {len(events)} events from kafka topic={kafka_topic}")
    log_metric(
        f"KAFKA_METRICS consumed={len(events)} elapsed_sec={METRICS['kafka_consumption_elapsed']:.2f} "
        f"rate_events_sec={consumption_rate:.2f}"
    )

    return events


def getenv_int(name: str, default: int) -> int:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    return int(raw)


def getenv_float(name: str, default: float) -> float:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    return float(raw)


def main():
    init_logging_paths()

    # === NEW: Start pipeline timer ===
    METRICS["pipeline_start"] = time.time()

    tenant_id = os.getenv("TENANT_ID", "tenant2").strip() or "tenant2"

    kafka_brokers = os.getenv("KAFKA_BROKERS", DEFAULT_KAFKA_BROKERS).strip() or DEFAULT_KAFKA_BROKERS
    kafka_topic = os.getenv("KAFKA_TOPIC", DEFAULT_KAFKA_TOPIC).strip() or DEFAULT_KAFKA_TOPIC
    kafka_group = os.getenv("KAFKA_CONSUMER_GROUP", DEFAULT_KAFKA_GROUP).strip() or DEFAULT_KAFKA_GROUP

    max_events = max(0, getenv_int("MAX_EVENTS", 1000))
    kafka_idle_timeout_ms = max(1000, getenv_int("KAFKA_IDLE_TIMEOUT_MS", 5000))
    out_of_order_minutes = max(0, getenv_int("OUT_OF_ORDER_MINUTES", 3))
    window_size_minutes = max(1, getenv_int("WINDOW_SIZE_MINUTES", 15))
    window_slide_seconds = max(1, getenv_int("WINDOW_SLIDE_SECONDS", 60))
    parallelism = max(1, getenv_int("PARALLELISM", 1))
    callback_url = os.getenv("TENANT_CALLBACK_URL", "").strip()

    cassandra_hosts_raw = os.getenv("CASSANDRA_HOSTS", DEFAULT_CASSANDRA_HOSTS).strip() or DEFAULT_CASSANDRA_HOSTS
    cassandra_hosts = split_csv(cassandra_hosts_raw)
    cassandra_keyspace = os.getenv("CASSANDRA_KEYSPACE", DEFAULT_CASSANDRA_KEYSPACE).strip() or DEFAULT_CASSANDRA_KEYSPACE

    temp_low = getenv_float("TEMP_ALERT_LOW", 12.0)
    temp_high = getenv_float("TEMP_ALERT_HIGH", 35.0)
    hum_low = getenv_float("HUM_ALERT_LOW", 20.0)
    hum_high = getenv_float("HUM_ALERT_HIGH", 95.0)

    log_info(
        (
            "INFO streamanalyticsapp start "
            f"tenant_id={tenant_id} kafka_brokers={kafka_brokers} kafka_topic={kafka_topic} "
            f"max_events={max_events} out_of_order_minutes={out_of_order_minutes} "
            f"window_size_minutes={window_size_minutes} window_slide_seconds={window_slide_seconds} "
            f"parallelism={parallelism} "
            f"callback_enabled={bool(callback_url)} cassandra_keyspace={cassandra_keyspace}"
        )
    )
    log_info(f"INFO logs: app={APP_LOG_PATH} analytics={ANALYTICS_LOG_PATH} metrics={METRICS_LOG_PATH}")

    env = StreamExecutionEnvironment.get_execution_environment()
    env.set_parallelism(parallelism)
    env.set_python_executable(sys.executable)

    source_type = Types.TUPLE([
        Types.INT(),
        Types.STRING(),
        Types.STRING(),
        Types.FLOAT(),
        Types.FLOAT(),
        Types.LONG(),
        Types.FLOAT(),
        Types.FLOAT(),
    ])

    events = load_events_from_kafka(
        kafka_brokers=kafka_brokers,
        kafka_topic=kafka_topic,
        kafka_group=kafka_group,
        max_events=max_events,
        idle_timeout_ms=kafka_idle_timeout_ms,
    )
    stream = env.from_collection(events, type_info=source_type)

    # === NEW: Mark window processing start ===
    METRICS["window_processing_start"] = time.time()

    wm = (
        WatermarkStrategy
        .for_bounded_out_of_orderness(Duration.of_minutes(out_of_order_minutes))
        .with_timestamp_assigner(EventTimestampAssigner())
    )

    analytics = (
        stream
        .assign_timestamps_and_watermarks(wm)
        .key_by(SensorKeySelector(), Types.INT())
        .window(SlidingEventTimeWindows.of(Time.minutes(window_size_minutes), Time.seconds(window_slide_seconds)))
        .process(
            AnalyticsWindowFunction(tenant_id, temp_low, temp_high, hum_low, hum_high),
            output_type=Types.STRING(),
        )
    )

    analytics.map(
        CallbackMapFunction(callback_url, cassandra_hosts, cassandra_keyspace),
        output_type=Types.STRING(),
    ).print()

    try:
        env.execute("tenant2-streamanalyticsapp")
    finally:
        # === NEW: Log final metrics summary ===
        pipeline_end = time.time()
        pipeline_total_time = pipeline_end - METRICS["pipeline_start"]

        avg_write_ms = (sum(METRICS["cassandra_write_times_ms"]) / len(METRICS["cassandra_write_times_ms"])
                        if METRICS["cassandra_write_times_ms"] else 0)
        avg_window_size = (sum(METRICS["window_sizes"]) / len(METRICS["window_sizes"])
                           if METRICS["window_sizes"] else 0)
        amplification_factor = (METRICS["cassandra_writes_successful"] / METRICS["kafka_events_consumed"]
                                if METRICS["kafka_events_consumed"] > 0 else 0)

        log_metric("=" * 80)
        log_metric("FINAL PERFORMANCE SUMMARY")
        log_metric("=" * 80)
        log_metric(f"kafka_events_consumed={METRICS['kafka_events_consumed']}")
        log_metric(f"kafka_consumption_elapsed_sec={METRICS['kafka_consumption_elapsed']:.2f}")
        log_metric(f"kafka_consumption_rate_events_sec={METRICS['kafka_events_consumed']/METRICS['kafka_consumption_elapsed'] if METRICS['kafka_consumption_elapsed'] > 0 else 0:.2f}")
        log_metric(f"windows_emitted={METRICS['windows_emitted']}")
        log_metric(f"avg_window_size={avg_window_size:.1f}")
        log_metric(f"cassandra_writes_successful={METRICS['cassandra_writes_successful']}")
        log_metric(f"cassandra_writes_failed={METRICS['cassandra_writes_failed']}")
        if METRICS["cassandra_write_times_ms"]:
            log_metric(f"cassandra_avg_write_ms={avg_write_ms:.2f}")
            log_metric(f"cassandra_max_write_ms={max(METRICS['cassandra_write_times_ms']):.2f}")
            log_metric(f"cassandra_min_write_ms={min(METRICS['cassandra_write_times_ms']):.2f}")
        log_metric(f"amplification_factor={amplification_factor:.2f}")
        log_metric(f"total_pipeline_time_sec={pipeline_total_time:.2f}")
        log_metric("=" * 80)


if __name__ == "__main__":
    main()
