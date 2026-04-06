import csv
import json
import os
import time
from datetime import datetime, timezone

from confluent_kafka import Producer

DEFAULT_SOURCE_CSV = "../data/tenant2/2025-06-01_dht22.csv"
DEFAULT_KAFKA_BROKERS = "localhost:9094"
DEFAULT_KAFKA_TOPIC = "dht22-measurements"
DEFAULT_LOG_DIR = "./logs/kafka_producer"


def split_csv(value: str) -> list[str]:
    return [part.strip() for part in value.split(",") if part.strip()]


def init_log_file() -> str:
    log_dir = os.getenv("PRODUCER_LOG_DIR", DEFAULT_LOG_DIR).strip() or DEFAULT_LOG_DIR
    os.makedirs(log_dir, exist_ok=True)
    return os.path.join(log_dir, "producer.log")


def log_line(log_path: str, message: str) -> None:
    ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    print(message, flush=True)
    with open(log_path, "a", encoding="utf-8") as f:
        f.write(f"{ts} {message}\n")


def normalize_row_keys(row: dict[str, str]) -> dict[str, str]:
    normalized: dict[str, str] = {}
    for key, value in row.items():
        normalized_key = key.strip().lstrip("\ufeff")
        normalized[normalized_key] = value
    return normalized


def detect_csv_delimiter(file_path: str) -> str:
    with open(file_path, "r", encoding="utf-8") as f:
        sample = f.read(4096)
    if not sample:
        return ","
    try:
        dialect = csv.Sniffer().sniff(sample, delimiters=",;")
        return dialect.delimiter
    except csv.Error:
        header = sample.splitlines()[0] if sample.splitlines() else ""
        return ";" if ";" in header else ","


def parse_row(row: dict[str, str]) -> dict[str, object]:
    row = normalize_row_keys(row)
    required = ["sensor_id", "sensor_type", "location", "lat", "lon", "timestamp", "temperature", "humidity"]
    for key in required:
        if key not in row or row[key] is None or row[key].strip() == "":
            raise ValueError(f"missing or empty column: {key}")

    timestamp = row["timestamp"].strip()
    dt = datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%S")

    return {
        "sensor_id": int(row["sensor_id"].strip()),
        "sensor_type": row["sensor_type"].strip(),
        "location": row["location"].strip(),
        "lat": float(row["lat"].strip()),
        "lon": float(row["lon"].strip()),
        "timestamp": timestamp,
        "day": dt.strftime("%Y-%m-%d"),
        "hour": int(dt.strftime("%H")),
        "temperature": float(row["temperature"].strip()),
        "humidity": float(row["humidity"].strip()),
    }


def main() -> None:
    log_path = init_log_file()

    source_csv = os.getenv("SOURCE_CSV", DEFAULT_SOURCE_CSV).strip() or DEFAULT_SOURCE_CSV
    kafka_brokers = os.getenv("KAFKA_BROKERS", DEFAULT_KAFKA_BROKERS).strip() or DEFAULT_KAFKA_BROKERS
    kafka_topic = os.getenv("KAFKA_TOPIC", DEFAULT_KAFKA_TOPIC).strip() or DEFAULT_KAFKA_TOPIC
    max_events_raw = os.getenv("MAX_EVENTS", "").strip()
    if not max_events_raw:
        max_events_raw = os.getenv("MAX_MESSAGES", "0").strip()
    max_events = int(max_events_raw or "0")
    emit_delay_ms = int(os.getenv("EMIT_DELAY_MS", "0").strip() or "0")

    producer = Producer(
        {
            "bootstrap.servers": ",".join(split_csv(kafka_brokers)),
            "acks": "all",
            "client.id": "tenant2-csv-producer",
        }
    )

    produced = 0
    skipped = 0

    log_line(log_path, f"INFO producer start source_csv={source_csv} kafka_brokers={kafka_brokers} topic={kafka_topic} max_events={max_events} emit_delay_ms={emit_delay_ms}")

    # === START TIMING ===
    producer_start_time = time.time()
    first_event_time = None
    last_event_time = None

    delimiter = detect_csv_delimiter(source_csv)
    log_line(log_path, f"INFO detected csv delimiter='{delimiter}'")

    with open(source_csv, "r", encoding="utf-8") as f:
        reader = csv.DictReader(f, delimiter=delimiter)
        for row in reader:
            if max_events > 0 and produced + skipped >= max_events:
                break

            try:
                event = parse_row(row)
                payload = json.dumps(event, separators=(",", ":"))
                producer.produce(kafka_topic, payload.encode("utf-8"))
                produced += 1
                last_event_time = time.time()
                if first_event_time is None:
                    first_event_time = last_event_time

                # Log every 100 events
                if produced % 100 == 0:
                    elapsed = time.time() - producer_start_time
                    rate = produced / elapsed if elapsed > 0 else 0
                    emit_lag_actual = (last_event_time - first_event_time) * 1000 if first_event_time else 0
                    log_line(log_path, f"PROGRESS produced={produced} elapsed_sec={elapsed:.2f} rate_events_sec={rate:.2f} actual_emit_lag_ms={emit_lag_actual:.0f}")

                if emit_delay_ms > 0:
                    time.sleep(emit_delay_ms / 1000.0)

            except ValueError as exc:
                if skipped < 5:
                    log_line(log_path, f"WARN row {produced + skipped} skipped: {exc}")
                skipped += 1
                if skipped % 1000 == 0:
                    log_line(log_path, f"WARN total skipped: {skipped}")

    producer.flush(20)

    # === END TIMING ===
    producer_end_time = time.time()
    total_elapsed = producer_end_time - producer_start_time
    actual_emit_lag_ms = (last_event_time - first_event_time) * 1000 if first_event_time and last_event_time else 0

    production_rate = produced / total_elapsed if total_elapsed > 0 else 0
    avg_emit_delay_ms = (actual_emit_lag_ms / produced) if produced > 0 else 0

    log_line(
        log_path,
        f"SUMMARY produced={produced} skipped={skipped} "
        f"elapsed_sec={total_elapsed:.2f} rate_events_sec={production_rate:.2f} "
        f"actual_emit_lag_total_ms={actual_emit_lag_ms:.0f} avg_emit_delay_ms={avg_emit_delay_ms:.2f}",
    )
    log_line(log_path, "INFO producer exit")


if __name__ == "__main__":
    main()
