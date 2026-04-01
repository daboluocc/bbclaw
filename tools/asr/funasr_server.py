#!/usr/bin/env python3
"""
FunASR 常驻 HTTP 服务：仅依赖 Python 标准库（无 Flask），避免 venv 与 pip 不一致导致找不到包。

  export ASR_PROVIDER=openai_compatible
  export ASR_BASE_URL=http://127.0.0.1:18081
  export ASR_API_KEY=dummy
  export ASR_MODEL=funasr
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import tempfile
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Type

from funasr_core import transcribe_file, warm


def _extract_multipart_file(body: bytes, content_type: str) -> bytes:
    """从 multipart/form-data 中取第一个 name=\"file\" 的文件体。"""
    if not body:
        raise ValueError("empty body")
    m = re.search(r"boundary=([^;\s]+)", content_type, re.I)
    if not m:
        raise ValueError("not multipart or missing boundary")
    boundary = m.group(1).strip().strip('"').encode("ascii", errors="replace")
    delim = b"--" + boundary
    parts = body.split(delim)
    for part in parts:
        if b'name="file"' not in part and b"name='file'" not in part:
            continue
        sep = part.find(b"\r\n\r\n")
        if sep == -1:
            sep = part.find(b"\n\n")
            if sep == -1:
                continue
            raw = part[sep + 2 :]
        else:
            raw = part[sep + 4 :]
        raw = raw.rstrip(b"\r\n").rstrip(b"\n")
        if raw.endswith(b"--"):
            raw = raw[:-2].rstrip()
        if raw:
            return raw
    raise ValueError('no form field name="file"')


def _handler_factory(engine_arg: str) -> Type[BaseHTTPRequestHandler]:
    class FunASRHandler(BaseHTTPRequestHandler):
        def log_message(self, fmt: str, *args) -> None:
            return

        def _send_json(self, code: int, obj: dict) -> None:
            b = json.dumps(obj).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)

        def _send_text(self, code: int, text: str, content_type: str) -> None:
            b = text.encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)

        def do_GET(self) -> None:
            p = self.path.split("?", 1)[0].rstrip("/") or "/"
            if p == "/healthz":
                self._send_text(200, "ok", "text/plain; charset=utf-8")
                return
            if p == "/v1/models":
                self._send_json(
                    200,
                    {"object": "list", "data": [{"id": "funasr-sensevoice", "object": "model"}]},
                )
                return
            self.send_error(404, "Not Found")

        def do_POST(self) -> None:
            p = self.path.split("?", 1)[0].rstrip("/") or "/"
            if p != "/v1/audio/transcriptions":
                self.send_error(404, "Not Found")
                return
            auth = self.headers.get("Authorization", "")
            if not auth.startswith("Bearer "):
                self._send_json(401, {"error": {"message": "missing bearer"}})
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                length = 0
            body = self.rfile.read(length) if length > 0 else b""
            ct = self.headers.get("Content-Type", "")
            try:
                audio = _extract_multipart_file(body, ct)
            except ValueError as e:
                self._send_json(400, {"error": {"message": str(e)}})
                return

            suffix = ".wav"
            if b"RIFF" in audio[:12] and b"WAVE" in audio[:12]:
                suffix = ".wav"
            fd, path = tempfile.mkstemp(suffix=suffix)
            os.close(fd)
            try:
                with open(path, "wb") as f:
                    f.write(audio)
                text = transcribe_file(
                    path,
                    language=os.environ.get("FUNASR_LANGUAGE", "auto"),
                    model_arg=engine_arg,
                )
                self._send_json(200, {"text": text})
            finally:
                try:
                    os.remove(path)
                except OSError:
                    pass

    return FunASRHandler


def main() -> None:
    parser = argparse.ArgumentParser(description="FunASR resident server (OpenAI-compatible, stdlib HTTP)")
    parser.add_argument("--host", default=os.environ.get("FUNASR_SERVER_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("FUNASR_SERVER_PORT", "18081")))
    parser.add_argument(
        "-m",
        "--model",
        default=os.environ.get("FUNASR_SERVER_MODEL", "dummy"),
        help="dummy -> SenseVoice; paraformer-zh -> Paraformer 中文",
    )
    args = parser.parse_args()

    print(f"Warming FunASR (model_arg={args.model})...", file=sys.stderr)
    warm(args.model)
    print(f"Ready. http://{args.host}:{args.port}/v1/audio/transcriptions", file=sys.stderr)
    print("Adapter: ASR_PROVIDER=openai_compatible ASR_BASE_URL=http://127.0.0.1:18081 ASR_API_KEY=dummy", file=sys.stderr)

    handler = _handler_factory(args.model)
    server = HTTPServer((args.host, args.port), handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nShutting down.", file=sys.stderr)
        server.shutdown()


if __name__ == "__main__":
    main()
