import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        content_length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(content_length)

        try:
            payload = json.loads(body.decode("utf-8"))
            print(json.dumps(payload, indent=2), flush=True)
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        except Exception as exc:
            print(f"Invalid payload: {exc}", flush=True)
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"invalid")

    def log_message(self, format, *args):
        return


def main():
    host = "127.0.0.1"
    port = 8080
    server = HTTPServer((host, port), Handler)
    print(f"Receiver listening on http://{host}:{port}/tenant2/analytics", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
