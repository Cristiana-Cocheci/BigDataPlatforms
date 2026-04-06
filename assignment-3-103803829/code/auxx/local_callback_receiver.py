import json
import os
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer

DEFAULT_LOG_DIR = "./logs/receiver"
RECEIVER_LOG_PATH = ""


def init_logging_paths() -> None:
    global RECEIVER_LOG_PATH

    log_dir = os.getenv("RECEIVER_LOG_DIR", DEFAULT_LOG_DIR).strip() or DEFAULT_LOG_DIR
    os.makedirs(log_dir, exist_ok=True)
    RECEIVER_LOG_PATH = os.path.join(log_dir, "receiver.log")


def log_line(message: str) -> None:
    timestamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    print(message, flush=True)
    if RECEIVER_LOG_PATH:
        with open(RECEIVER_LOG_PATH, "a", encoding="utf-8") as f:
            f.write(f"{timestamp} {message}\n")


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        content_length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(content_length)

        try:
            payload = json.loads(body.decode("utf-8"))
            log_line(json.dumps(payload, separators=(",", ":")))
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        except Exception as exc:
            log_line(f"Invalid payload: {exc}")
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"invalid")

    def log_message(self, format, *args):
        return


def main():
    init_logging_paths()
    host = os.getenv("RECEIVER_HOST", "127.0.0.1").strip() or "127.0.0.1"
    port = int(os.getenv("RECEIVER_PORT", "8080").strip() or "8080")
    server = HTTPServer((host, port), Handler)
    log_line(f"Receiver logging to {RECEIVER_LOG_PATH}")
    log_line(f"Receiver listening on http://{host}:{port}/tenant2/analytics")
    server.serve_forever()


if __name__ == "__main__":
    main()
