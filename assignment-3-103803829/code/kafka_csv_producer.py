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


def parse_row(row: dict[str, str]) -> dict[str, object]:
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

    log_line(log_path, f"INFO producer start source_csv={source_csv} kafka_brokers={kafka_brokers} topic={kafka_topic}")

    with open(source_csv, "r", encoding="utf-8") as f:
        reader = csv.DictReader(f, delimiter=";")
        for line_no, row in enumerate(reader, start=2):
            try:
                payload = parse_row(row)
            except ValueError as exc:
                skipped += 1
                if skipped <= 5:
                    log_line(log_path, f"WARN skipping malformed csv row line={line_no}: {exc}")
                continue

            while True:
                try:
                    producer.produce(
                        topic=kafka_topic,
                        key=str(payload["sensor_id"]).encode("utf-8"),
                        value=json.dumps(payload, separators=(",", ":")).encode("utf-8"),
                    )
                    break
                except BufferError:
                    # Allow the client to deliver in-flight messages before retrying.
                    producer.poll(0.2)
            producer.poll(0)
            produced += 1

            if emit_delay_ms > 0:
                time.sleep(emit_delay_ms / 1000.0)

            if max_events > 0 and produced >= max_events:
                break

    producer.flush(20)

    log_line(log_path, f"INFO producer done produced={produced} skipped={skipped}")


if __name__ == "__main__":
    main()
