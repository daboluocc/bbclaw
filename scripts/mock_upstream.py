#!/usr/bin/env python3
import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, payload):
        raw = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self):
        if self.path == "/v1/audio/transcriptions":
            self._json(200, {"text": "mock transcript from upstream"})
            return
        if self.path == "/rpc":
            self._json(
                200,
                {
                    "jsonrpc": "2.0",
                    "id": "bbclaw-adapter",
                    "result": {"ok": True},
                },
            )
            return
        self._json(404, {"error": "not found"})

    def do_GET(self):
        if self.path == "/healthz":
            self._json(200, {"ok": True})
            return
        self._json(404, {"error": "not found"})

    def log_message(self, fmt, *args):
        # keep CI/local output clean
        return


if __name__ == "__main__":
    server = HTTPServer(("127.0.0.1", 19091), Handler)
    print("mock upstream listening on 127.0.0.1:19091")
    server.serve_forever()
